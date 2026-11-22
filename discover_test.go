package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSeedWeight(t *testing.T) {
	cases := []struct {
		name   string
		seed   Seed
		rating int
		want   float64
	}{
		{"watched", Seed{Watched: true}, 0, weightWatched},
		{"requested only", Seed{Requested: true}, 0, weightRequested},
		{"watched and requested counts as watched", Seed{Watched: true, Requested: true}, 0, weightWatched},
		{"liked beats watched", Seed{Watched: true}, 1, weightLiked},
		{"disliked overrides watched", Seed{Watched: true}, -1, weightDisliked},
		{"disliked overrides watched and requested", Seed{Watched: true, Requested: true}, -1, weightDisliked},
		{"starred only", Seed{Starred: true}, 0, weightStarred},
		{"requested beats starred", Seed{Requested: true, Starred: true}, 0, weightRequested},
		{"watched beats starred", Seed{Watched: true, Starred: true}, 0, weightWatched},
		{"disliked overrides starred", Seed{Starred: true}, -1, weightDisliked},
		{"neither watched nor requested weighs nothing", Seed{}, 0, 0},
	}
	for _, c := range cases {
		if got := seedWeight(c.seed, c.rating); got != c.want {
			t.Errorf("%s: weight %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWithStarred(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	seeds := []Seed{{Key: Key{Movie, 1}, Title: "Fall", Watched: true, At: t0}}
	got := withStarred(seeds, []Title{mv(1, "Fall"), mv(5, "Greenland 2"), mv(5, "Greenland 2")}, now)
	if len(got) != 2 {
		t.Fatalf("seeds = %v, want Fall and Greenland 2 once each", got)
	}
	if g := got[0]; !g.Watched || !g.Starred || !g.At.Equal(t0) {
		t.Errorf("a watched title also starred = %+v, want it watched, starred, with its own date", g)
	}
	if g := got[1]; g.Key != (Key{Movie, 5}) || g.Title != "Greenland 2" || g.Watched || g.Requested || !g.Starred || !g.At.Equal(now) {
		t.Errorf("a title only starred = %+v, want starred as of now", g)
	}
	if seeds[0].Starred {
		t.Error("withStarred changed the seeds it was given")
	}
	if b := becauseOf(got[1]); b.Reason != "starred" {
		t.Errorf("because of a starred title: %q", b.Reason)
	}
}

// fakeRecs answers recommendations from a map; a key in fail errors.
type fakeRecs struct {
	recs map[Key][]Title
	fail map[Key]bool
}

func (f fakeRecs) Recommendations(_ context.Context, k Key) ([]Title, error) {
	if f.fail[k] {
		return nil, errors.New("seerr down")
	}
	return f.recs[k], nil
}

func mv(id int, title string) Title { return Title{Type: Movie, ID: id, Title: title} }

func noExclusions() Exclusions {
	return Exclusions{statuses: map[Key]int{}, own: map[Key]bool{}, blocked: map[Key]bool{}}
}

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func titlesOf(cards []Card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.Title.Title
	}
	return out
}

func TestBuildWallRanksBySummedWeight(t *testing.T) {
	zodiac, prisoners, dune := Key{Movie, 1}, Key{Movie, 2}, Key{Movie, 3}
	rec := fakeRecs{recs: map[Key][]Title{
		zodiac:    {mv(10, "Sicario"), mv(11, "Arrival")},
		prisoners: {mv(10, "Sicario")},
		dune:      {mv(11, "Arrival")},
	}}
	seeds := []Seed{
		{Key: zodiac, Title: "Zodiac", Watched: true, At: t0},
		{Key: prisoners, Title: "Prisoners", Watched: true, At: t0.Add(time.Hour)},
		{Key: dune, Title: "Dune", Requested: true, At: t0.Add(2 * time.Hour)},
	}
	cards, failed := buildWall(context.Background(), rec, seeds, nil, noExclusions())
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}
	// Sicario: 1.0 + 1.0 = 2.0; Arrival: 1.0 + 0.8 = 1.8.
	if got := titlesOf(cards); len(got) != 2 || got[0] != "Sicario" || got[1] != "Arrival" {
		t.Fatalf("order %v, want [Sicario Arrival]", got)
	}
	if cards[0].Score != 2.0 {
		t.Errorf("Sicario score %v, want 2", cards[0].Score)
	}
	// Arrival's strongest seed is Zodiac (watched, 1.0), not Dune (requested, 0.8).
	if b := cards[1].Because; b.Title != "Zodiac" || b.Reason != "watched" {
		t.Errorf("Arrival because %+v, want watched Zodiac", b)
	}
	// Between equal seeds the latest wins: Prisoners is newer than Zodiac.
	if b := cards[0].Because; b.Title != "Prisoners" {
		t.Errorf("Sicario because %+v, want Prisoners", b)
	}
}

func TestBuildWallDislikeSubtractsAndCanRemove(t *testing.T) {
	liked, disliked := Key{Movie, 1}, Key{Movie, 2}
	rec := fakeRecs{recs: map[Key][]Title{
		liked:    {mv(10, "Shared"), mv(11, "Only liked")},
		disliked: {mv(10, "Shared"), mv(12, "Only disliked")},
	}}
	seeds := []Seed{
		{Key: liked, Title: "Liked", Watched: true, At: t0},
		{Key: disliked, Title: "Disliked", Watched: true, At: t0},
	}
	cards, _ := buildWall(context.Background(), rec, seeds, map[Key]int{disliked: -1}, noExclusions())
	got := titlesOf(cards)
	// Shared: 1.0 - 1.0 = 0, so gone; Only disliked never had a positive seed.
	if len(got) != 1 || got[0] != "Only liked" {
		t.Fatalf("wall %v, want only [Only liked]", got)
	}
	// Without the dislike, Shared is there: the rating did the removing.
	cards, _ = buildWall(context.Background(), rec, seeds, nil, noExclusions())
	if len(cards) != 3 {
		t.Fatalf("without the dislike the wall has %v, want 3 titles", titlesOf(cards))
	}
}

func TestBuildWallExclusions(t *testing.T) {
	seed := Key{Movie, 1}
	inline := mv(14, "Seerr says requested")
	inline.Status = statusProcessing
	deleted := mv(15, "Deleted from Seerr")
	rec := fakeRecs{recs: map[Key][]Title{seed: {
		mv(10, "In library"), mv(11, "Watched"), mv(12, "Blocked"), mv(13, "New"), inline, deleted,
		{Type: TV, ID: 10, Title: "Show with the same ID as the library movie"},
	}}}
	ex := Exclusions{
		statuses: map[Key]int{{Movie, 10}: statusAvailable, {Movie, 15}: statusDeleted},
		own:      map[Key]bool{{Movie, 11}: true},
		blocked:  map[Key]bool{{Movie, 12}: true},
	}
	cards, _ := buildWall(context.Background(), rec, []Seed{{Key: seed, Watched: true, At: t0}}, nil, ex)
	want := map[string]bool{"New": true, "Deleted from Seerr": true, "Show with the same ID as the library movie": true}
	got := titlesOf(cards)
	if len(got) != len(want) {
		t.Fatalf("wall %v, want exactly %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("%q is on the wall but must be hidden", g)
		}
	}
}

func TestBuildWallSkipsFailedSeedsButKeepsTheRest(t *testing.T) {
	ok, broken := Key{Movie, 1}, Key{Movie, 2}
	rec := fakeRecs{
		recs: map[Key][]Title{ok: {mv(10, "Still here")}},
		fail: map[Key]bool{broken: true},
	}
	seeds := []Seed{{Key: ok, Watched: true, At: t0}, {Key: broken, Watched: true, At: t0}}
	cards, failed := buildWall(context.Background(), rec, seeds, nil, noExclusions())
	if failed != 1 || len(cards) != 1 {
		t.Fatalf("failed=%d cards=%v, want 1 failure and [Still here]", failed, titlesOf(cards))
	}
}

func TestBuildWallUsesOnlyTheMostRecentSeeds(t *testing.T) {
	recs := map[Key][]Title{}
	var seeds []Seed
	for i := 1; i <= wallMaxSeeds+5; i++ {
		k := Key{Movie, i}
		recs[k] = []Title{mv(1000+i, "from seed")}
		seeds = append(seeds, Seed{Key: k, Watched: true, At: t0.Add(time.Duration(i) * time.Hour)})
	}
	cards, _ := buildWall(context.Background(), fakeRecs{recs: recs}, seeds, nil, noExclusions())
	if len(cards) != wallMaxSeeds {
		t.Fatalf("%d cards, want %d (one per used seed)", len(cards), wallMaxSeeds)
	}
	for _, c := range cards {
		if c.ID <= 1005 { // seeds 1..5 are the oldest and must be left out
			t.Errorf("card %d comes from one of the oldest seeds", c.ID)
		}
	}
}

func TestExplainMatchesTheWall(t *testing.T) {
	zodiac, prisoners, dune, bad := Key{Movie, 1}, Key{Movie, 2}, Key{Movie, 3}, Key{Movie, 4}
	target := Key{Movie, 10}
	rec := fakeRecs{recs: map[Key][]Title{
		zodiac:    {mv(9, "Other"), mv(10, "Sicario")}, // 2nd of 2
		prisoners: {mv(10, "Sicario")},                 // 1st of 1
		dune:      {mv(10, "Sicario"), mv(11, "X"), mv(12, "Y")},
		bad:       {mv(10, "Sicario")},
	}}
	seeds := []Seed{
		{Key: zodiac, Title: "Zodiac", Watched: true, At: t0},
		{Key: prisoners, Title: "Prisoners", Watched: true, At: t0},
		{Key: dune, Title: "Dune", Requested: true, At: t0},
		{Key: bad, Title: "Disliked one", Watched: true, At: t0},
	}
	ratings := map[Key]int{zodiac: 1, bad: -1}
	score, by, failed := explain(context.Background(), rec, seeds, ratings, target)
	if failed != 0 || len(by) != 4 {
		t.Fatalf("failed=%d by=%+v", failed, by)
	}
	// Strongest first: liked 1.5, watched 1.0, requested 0.8, disliked -1.0.
	want := []string{"Zodiac liked 1.5 2/2", "Prisoners watched 1 1/1", "Dune requested 0.8 1/3", "Disliked one disliked -1 1/1"}
	for i, c := range by {
		if got := fmt.Sprintf("%s %s %v %d/%d", c.Title, c.Reason, c.Weight, c.Position, c.Of); got != want[i] {
			t.Errorf("contribution %d = %q, want %q", i, got, want[i])
		}
	}
	// The same number the wall shows for it.
	cards, _ := buildWall(context.Background(), rec, seeds, ratings, noExclusions())
	for _, c := range cards {
		if c.Key() == target && c.Score != score {
			t.Errorf("explain says %v, the wall %v", score, c.Score)
		}
	}
	if score != 1.5+1.0+0.8-1.0 {
		t.Errorf("score %v", score)
	}
	// A title nothing recommends scores nothing, with nothing to show.
	if s, by, _ := explain(context.Background(), rec, seeds, ratings, Key{TV, 10}); s != 0 || len(by) != 0 {
		t.Errorf("unrecommended title: %v %+v", s, by)
	}
}

func TestBuildWallIgnoresSeedsWithoutWeight(t *testing.T) {
	k := Key{Movie, 1}
	rec := fakeRecs{recs: map[Key][]Title{k: {mv(10, "X")}}}
	cards, _ := buildWall(context.Background(), rec, []Seed{{Key: k, At: t0}}, nil, noExclusions())
	if len(cards) != 0 {
		t.Fatalf("a seed that is neither watched nor requested produced %v", titlesOf(cards))
	}
}
