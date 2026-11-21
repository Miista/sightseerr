package main

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// Seed weights (README, "Weighting"). A seed has one weight, its
// strongest signal; a dislike overrides everything. A recommendation scores
// the sum of the weights of the seeds recommending it.
const (
	weightLiked     = 1.5
	weightWatched   = 1.0
	weightRequested = 0.8
	weightDisliked  = -1.0
)

func seedWeight(s Seed, rating int) float64 {
	switch {
	case rating < 0:
		return weightDisliked
	case rating > 0:
		return weightLiked
	case s.Watched:
		return weightWatched
	case s.Requested:
		return weightRequested
	}
	return 0
}

// Because is the seed a card is shown for: "because you watched X".
type Because struct {
	Type   MediaType `json:"type"`
	ID     int       `json:"id"`
	Title  string    `json:"title"`
	Reason string    `json:"reason"` // "watched" or "requested"
}

func becauseOf(s Seed) Because {
	reason := "requested"
	if s.Watched {
		reason = "watched" // it was actually seen
	}
	return Because{Type: s.Key.Type, ID: s.Key.ID, Title: s.Title, Reason: reason}
}

// Card is one poster on the wall.
type Card struct {
	Title
	Score   float64 `json:"score"`
	Because Because `json:"because"`
}

// Exclusions is what must never be offered: titles Seerr knows (had, on the
// way, blocklisted), titles the person watched or requested, and titles they
// blocked.
type Exclusions struct {
	statuses map[Key]int
	own      map[Key]bool
	blocked  map[Key]bool
}

// hides reports whether the title must not be shown. The status Seerr gave
// with the title counts too, since it can be newer than the last status sync.
func (e Exclusions) hides(t Title) bool {
	k := t.Key()
	return inLibrary(t.Status) || inLibrary(e.statuses[k]) || e.own[k] || e.blocked[k]
}

func (e Exclusions) filter(ts []Title) []Title {
	out := make([]Title, 0, len(ts))
	for _, t := range ts {
		if !e.hides(t) {
			out = append(out, t)
		}
	}
	return out
}

// recommender is what building the wall needs from Seerr; a cache in front of
// seerrClient in production, a fake in tests.
type recommender interface {
	Recommendations(ctx context.Context, k Key) ([]Title, error)
}

const (
	wallMaxSeeds = 80  // most recent positive seeds asked for recommendations
	wallMaxCards = 120 // posters on the wall
	wallFetchers = 4   // recommendation lookups in flight at once
)

// buildWall ranks the recommendations of the person's seeds. Positive seeds
// are the most recent wallMaxSeeds; disliked seeds always count, since they
// only take away. A seed whose recommendations can't be fetched is skipped
// (the wall is still worth showing); the error count says how many.
func buildWall(ctx context.Context, rec recommender, seeds []Seed, ratings map[Key]int, ex Exclusions) ([]Card, int) {
	type weighted struct {
		seed   Seed
		weight float64
	}
	var positive, negative []weighted
	for _, s := range seeds {
		switch w := seedWeight(s, ratings[s.Key]); {
		case w > 0:
			positive = append(positive, weighted{s, w})
		case w < 0:
			negative = append(negative, weighted{s, w})
		}
	}
	slices.SortStableFunc(positive, func(a, b weighted) int { return b.seed.At.Compare(a.seed.At) })
	if len(positive) > wallMaxSeeds {
		positive = positive[:wallMaxSeeds]
	}
	use := append(positive, negative...)

	results := make([][]Title, len(use))
	failed := make([]bool, len(use))
	var wg sync.WaitGroup
	sem := make(chan struct{}, wallFetchers)
	for i, w := range use {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ts, err := rec.Recommendations(ctx, w.seed.Key)
			results[i], failed[i] = ts, err != nil
		}()
	}
	wg.Wait()

	type acc struct {
		card     Card
		bestW    float64
		bestAt   time.Time
		latestAt time.Time
	}
	byKey := map[Key]*acc{}
	errs := 0
	for i, w := range use {
		if failed[i] {
			errs++
			continue
		}
		for _, t := range results[i] {
			if ex.hides(t) {
				continue
			}
			a, ok := byKey[t.Key()]
			if !ok {
				a = &acc{card: Card{Title: t}}
				byKey[t.Key()] = a
			}
			a.card.Score += w.weight
			if w.weight <= 0 {
				continue
			}
			// The card names its strongest seed; between equals, the latest.
			if w.weight > a.bestW || w.weight == a.bestW && w.seed.At.After(a.bestAt) {
				a.bestW, a.bestAt, a.card.Because = w.weight, w.seed.At, becauseOf(w.seed)
			}
			if w.seed.At.After(a.latestAt) {
				a.latestAt = w.seed.At
			}
		}
	}

	accs := make([]*acc, 0, len(byKey))
	for _, a := range byKey {
		if a.card.Score > 0 && a.bestW > 0 {
			accs = append(accs, a)
		}
	}
	slices.SortFunc(accs, func(a, b *acc) int {
		return cmp.Or(
			cmp.Compare(b.card.Score, a.card.Score),
			b.latestAt.Compare(a.latestAt),
			cmp.Compare(a.card.Title.Title, b.card.Title.Title),
			cmp.Compare(a.card.Key().String(), b.card.Key().String()),
		)
	})
	if len(accs) > wallMaxCards {
		accs = accs[:wallMaxCards]
	}
	cards := make([]Card, len(accs))
	for i, a := range accs {
		cards[i] = a.card
	}
	return cards, errs
}
