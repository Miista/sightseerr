package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// fakeJellyfinDB builds the two tables the backfill reads, shaped like
// Jellyfin 12's: a movie's placeholder rows (TMDb, IMDb, GUID keys sharing a
// user and last-played time), an episode's (GUID only), an unplayed one, and
// a live item.
func fakeJellyfinDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "jellyfin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE BaseItems (Id TEXT PRIMARY KEY, Type TEXT);
		CREATE TABLE UserData (ItemId TEXT, UserId TEXT, CustomDataKey TEXT, LastPlayedDate TEXT, Played INTEGER);
		INSERT INTO BaseItems VALUES ('ph', 'PLACEHOLDER'), ('live', 'MediaBrowser.Controller.Entities.Movies.Movie');
		INSERT INTO UserData VALUES
			('ph', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', '603',  '2026-11-18 19:38:00.9007713', 1),
			('ph', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', 'tt0133093', '2026-11-18 19:38:00.9007713', 1),
			('ph', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', '87466dd6-8f0e-46b1-7e69-45dae48feb38', '2026-11-18 19:38:00.9007713', 1),
			('ph', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', 'eec0e46e-d9c4-fae2-50fd-fc06a8c6f935', '2026-11-10 10:00:00.0000000', 1),
			('ph', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', '550',  '2026-11-01 10:00:00.0000000', 0),
			('live', '6C8A8E57-E8F9-44CD-AB6E-E6E4E433C8DD', '680', '2026-11-01 10:00:00.0000000', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReadPlaceholders(t *testing.T) {
	got, skipped, err := readPlaceholders(fakeJellyfinDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || skipped != 1 {
		t.Fatalf("got %+v, skipped %d; want the one movie, the episode skipped, unplayed and live rows ignored", got, skipped)
	}
	p := got[0]
	if p.num != "603" || p.imdb != "tt0133093" || p.jellyfinUser != "6c8a8e57e8f944cdab6ee6e4e433c8dd" || p.at.Day() != 18 {
		t.Fatalf("placeholder %+v", p)
	}
}

// fakeBackfillSeerr answers details by key and searches by exact query.
type fakeBackfillSeerr struct {
	details map[Key]Details
	search  map[string][]Title
	asked   []string
}

func (f *fakeBackfillSeerr) Details(_ context.Context, k Key) (Details, error) {
	f.asked = append(f.asked, k.String())
	if d, ok := f.details[k]; ok {
		return d, nil
	}
	return Details{}, errors.New("not found")
}

func (f *fakeBackfillSeerr) Search(_ context.Context, q string) ([]Title, error) {
	f.asked = append(f.asked, q)
	return f.search[q], nil
}

func TestResolvePlaceholderByIMDb(t *testing.T) {
	seerr := &fakeBackfillSeerr{details: map[Key]Details{
		{Movie, 1399}: {Title: Title{Type: Movie, ID: 1399, Title: "Some movie"}, IMDb: "tt9999999"},
		{TV, 1399}:    {Title: Title{Type: TV, ID: 1399, Title: "Game of Thrones"}, IMDb: "tt0944947"},
		{Movie, 603}:  {Title: Title{Type: Movie, ID: 603, Title: "The Matrix"}, IMDb: "tt0133093"},
	}}
	// The same TMDb ID exists as a movie and a show; the IMDb ID picks the show.
	if d, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "1399", imdb: "tt0944947"}); !ok || d.Key() != (Key{TV, 1399}) {
		t.Errorf("resolved to %v, %v; want tv:1399", d.Key(), ok)
	}
	if d, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "603", imdb: "tt0133093"}); !ok || d.Key() != (Key{Movie, 603}) {
		t.Errorf("resolved to %v, %v; want movie:603", d.Key(), ok)
	}
	// No IMDb match either way: not guessed.
	if d, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "603", imdb: "tt0000001"}); ok {
		t.Errorf("resolved %v despite no IMDb match", d.Key())
	}
}

func TestResolvePlaceholderEpisodeToItsShow(t *testing.T) {
	debris := Title{Type: TV, ID: 106159, Title: "Debris"}
	seerr := &fakeBackfillSeerr{search: map[string][]Title{
		"tvdb:383491":     {debris},
		"imdb:tt11640020": {{Type: Movie, ID: 5, Title: "A movie first"}, debris},
		"tvdb:111111":     {debris},
		"imdb:tt2222222":  {{Type: TV, ID: 999, Title: "Another show"}},
	}}
	// Episode 1x03 of Debris: both lookups agree, so the show counts.
	got, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "383491001003", imdb: "tt11640020001003"})
	if !ok || got.Key() != (Key{TV, 106159}) || got.Title != "Debris" {
		t.Fatalf("resolved to %+v, %v; want tv:106159 Debris", got, ok)
	}
	for _, q := range seerr.asked {
		if q == "movie:383491001003" || q == "tv:383491001003" {
			t.Errorf("asked Seerr for %s: an episode key is not a TMDb ID", q)
		}
	}
	// The two lookups disagree: not guessed.
	if got, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "111111001001", imdb: "tt2222222001001"}); ok {
		t.Errorf("resolved %+v though TVDB and IMDb point to different shows", got)
	}
	// Suffixes differ, so it's not an episode key at all.
	if got, ok := resolvePlaceholder(context.Background(), seerr, placeholderWatch{num: "383491001003", imdb: "tt11640020001004"}); ok {
		t.Errorf("resolved %+v from mismatched episode keys", got)
	}
}

func TestEpisodeKeys(t *testing.T) {
	cases := []struct {
		num, imdb, tvdb, series string
		ok                      bool
	}{
		{"383491001003", "tt11640020001003", "383491", "tt11640020", true},
		{"362696003008", "tt5180504003008", "362696", "tt5180504", true},
		{"603", "tt0133093", "", "", false},                 // a movie
		{"1063879", "tt26744289", "", "", false},            // a movie with a 7-digit ID
		{"383491001003", "tt11640020001009", "", "", false}, // suffixes differ
		{"001003", "tt001003", "", "", false},               // nothing left of the IDs
	}
	for _, c := range cases {
		tvdb, series, ok := episodeKeys(c.num, c.imdb)
		if ok != c.ok || tvdb != c.tvdb || series != c.series {
			t.Errorf("episodeKeys(%q, %q) = %q, %q, %v; want %q, %q, %v", c.num, c.imdb, tvdb, series, ok, c.tvdb, c.series, c.ok)
		}
	}
}
