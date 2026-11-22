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
	weightStarred   = 0.6 // on the person's Seerr watchlist: "maybe later"
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
	case s.Starred:
		return weightStarred
	}
	return 0
}

// withStarred adds the person's starred titles to their seeds. Seerr's
// watchlist has no dates, and a star is a wish held now, so a title that is
// only starred counts as of now; one also watched or requested keeps its own
// date (and its stronger weight).
func withStarred(seeds []Seed, starred []Title, now time.Time) []Seed {
	out := slices.Clone(seeds)
	at := make(map[Key]int, len(out))
	for i, s := range out {
		at[s.Key] = i
	}
	for _, t := range starred {
		if i, ok := at[t.Key()]; ok {
			out[i].Starred = true
			continue
		}
		at[t.Key()] = len(out)
		out = append(out, Seed{Key: t.Key(), Title: t.Title, Starred: true, At: now})
	}
	return out
}

// Because is the seed a card is shown for: "because you watched X".
type Because struct {
	Type   MediaType `json:"type"`
	ID     int       `json:"id"`
	Title  string    `json:"title"`
	Reason string    `json:"reason"` // "watched", "requested" or "starred"
}

func becauseOf(s Seed) Because {
	reason := "starred"
	switch {
	case s.Watched:
		reason = "watched" // it was actually seen
	case s.Requested:
		reason = "requested"
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
// blocked. Starred titles are hidden too, but only where starred is set: the
// wall leaves it set, a title page's "More like this" clears it.
type Exclusions struct {
	statuses map[Key]int
	own      map[Key]bool
	blocked  map[Key]bool
	starred  map[Key]bool
}

// hides reports whether the title must not be shown. The status Seerr gave
// with the title counts too, since it can be newer than the last status sync.
func (e Exclusions) hides(t Title) bool {
	k := t.Key()
	return inLibrary(t.Status) || inLibrary(e.statuses[k]) || e.own[k] || e.blocked[k] || e.starred[k]
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

// weighted is a seed with the weight it counts with.
type weighted struct {
	seed   Seed
	weight float64
}

// wallSeeds is the seeds the wall is built from: the most recent
// wallMaxSeeds positive ones, and every disliked one (they only take away).
func wallSeeds(seeds []Seed, ratings map[Key]int) []weighted {
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
	return append(positive, negative...)
}

// fetchRecommendations asks for every seed's recommendations, a few at a
// time; failed[i] says seed i's couldn't be fetched.
func fetchRecommendations(ctx context.Context, rec recommender, use []weighted) ([][]Title, []bool) {
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
	return results, failed
}

// Contribution is one of the person's titles recommending a title: how it
// counts, and where the title sits in its recommendations.
type Contribution struct {
	Type     MediaType `json:"type"`
	ID       int       `json:"id"`
	Title    string    `json:"title"`
	Reason   string    `json:"reason"` // "liked", "disliked", "watched", "requested" or "starred"
	Weight   float64   `json:"weight"`
	Position int       `json:"position"` // 1-based place in that title's list
	Of       int       `json:"of"`       // length of that list
}

// explain says why a title scores what it does on the wall: the same seeds
// and lists buildWall uses, strongest contribution first. failed counts seeds
// whose recommendations couldn't be fetched.
func explain(ctx context.Context, rec recommender, seeds []Seed, ratings map[Key]int, target Key) (score float64, out []Contribution, failed int) {
	use := wallSeeds(seeds, ratings)
	results, bad := fetchRecommendations(ctx, rec, use)
	out = []Contribution{}
	for i, w := range use {
		if bad[i] {
			failed++
			continue
		}
		for pos, t := range results[i] {
			if t.Key() != target {
				continue
			}
			reason := becauseOf(w.seed).Reason
			switch r := ratings[w.seed.Key]; {
			case r > 0:
				reason = "liked"
			case r < 0:
				reason = "disliked"
			}
			out = append(out, Contribution{Type: w.seed.Key.Type, ID: w.seed.Key.ID, Title: w.seed.Title, Reason: reason,
				Weight: w.weight, Position: pos + 1, Of: len(results[i])})
			score += w.weight
			break
		}
	}
	slices.SortStableFunc(out, func(a, b Contribution) int {
		return cmp.Or(cmp.Compare(b.Weight, a.Weight), cmp.Compare(a.Position, b.Position))
	})
	return score, out, failed
}

// buildWall ranks the recommendations of the person's seeds. Positive seeds
// are the most recent wallMaxSeeds; disliked seeds always count, since they
// only take away. A seed whose recommendations can't be fetched is skipped
// (the wall is still worth showing); the error count says how many.
func buildWall(ctx context.Context, rec recommender, seeds []Seed, ratings map[Key]int, ex Exclusions) ([]Card, int) {
	use := wallSeeds(seeds, ratings)
	results, failed := fetchRecommendations(ctx, rec, use)

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
