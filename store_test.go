package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "sightseerr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return t0 }
	return s
}

// A data folder from before the rename (spor.db) is taken over once, WAL and
// all; a folder that already has sightseerr.db is never touched.
func TestStorePath(t *testing.T) {
	write := func(t *testing.T, dir string, files map[string]string) {
		t.Helper()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(dir, name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	t.Run("old database moves over with its WAL", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{"spor.db": "db", "spor.db-wal": "wal", "spor.db-shm": "shm"})
		path, err := storePath(dir)
		if err != nil || path != filepath.Join(dir, "sightseerr.db") {
			t.Fatalf("storePath = %q, %v", path, err)
		}
		for name, want := range map[string]string{"sightseerr.db": "db", "sightseerr.db-wal": "wal", "sightseerr.db-shm": "shm",
			"spor.db": "<missing>", "spor.db-wal": "<missing>", "spor.db-shm": "<missing>"} {
			if got := read(dir, name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})
	t.Run("an existing sightseerr.db wins and spor.db is left alone", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, map[string]string{"sightseerr.db": "new", "spor.db": "old", "spor.db-wal": "oldwal"})
		if _, err := storePath(dir); err != nil {
			t.Fatal(err)
		}
		if read(dir, "sightseerr.db") != "new" || read(dir, "spor.db") != "old" || read(dir, "spor.db-wal") != "oldwal" || read(dir, "sightseerr.db-wal") != "<missing>" {
			t.Error("storePath touched files although sightseerr.db existed")
		}
	})
	t.Run("an empty folder gets a fresh database", func(t *testing.T) {
		dir := t.TempDir()
		path, err := storePath(dir)
		if err != nil || path != filepath.Join(dir, "sightseerr.db") || read(dir, "sightseerr.db") != "<missing>" {
			t.Errorf("storePath = %q, %v; it must not create anything itself", path, err)
		}
	})
}

func seedFor(t *testing.T, s *Store, user int, k Key) (Seed, bool) {
	t.Helper()
	seeds, err := s.Seeds(user)
	if err != nil {
		t.Fatal(err)
	}
	for _, sd := range seeds {
		if sd.Key == k {
			return sd, true
		}
	}
	return Seed{}, false
}

func TestWatchedIsKeptAndOnlyMovesForward(t *testing.T) {
	s := newTestStore(t)
	k := Key{Movie, 603}
	s.AddWatched(1, []Watched{{Key: k, Title: "The Matrix", At: t0.Add(2 * time.Hour)}})
	// An older play, and then a sync where Jellyfin no longer has it (reaped).
	s.AddWatched(1, []Watched{{Key: k, Title: "", At: t0}})
	s.AddWatched(1, nil)
	sd, ok := seedFor(t, s, 1, k)
	if !ok || !sd.Watched || sd.Title != "The Matrix" || !sd.At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("seed %+v (found %v), want watched The Matrix at the later time", sd, ok)
	}
	if _, ok := seedFor(t, s, 2, k); ok {
		t.Fatal("another person's history leaked into user 2")
	}
}

func TestRequestedKeepsTheFirstDateAndJoinsWatched(t *testing.T) {
	s := newTestStore(t)
	k := Key{TV, 1399}
	s.AddRequested(1, k, "", t0)
	s.AddRequested(1, k, "Game of Thrones", t0.Add(time.Hour))
	sd, _ := seedFor(t, s, 1, k)
	if !sd.Requested || sd.Watched || sd.Title != "Game of Thrones" || !sd.At.Equal(t0) {
		t.Fatalf("seed %+v, want requested (first date), not watched, named", sd)
	}
	s.AddWatched(1, []Watched{{Key: k, At: t0.Add(3 * time.Hour)}})
	sd, _ = seedFor(t, s, 1, k)
	if !sd.Requested || !sd.Watched || !sd.At.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("seed %+v, want both requested and watched, dated by the watch", sd)
	}
}

func TestAddWatchedIfMissingNeverTouchesKnownTitles(t *testing.T) {
	s := newTestStore(t)
	known := Key{Movie, 1}
	s.AddRequested(1, known, "Known", t0)
	ws := []Watched{{Key: known, Title: "Known", At: t0}, {Key: Key{Movie, 2}, Title: "New", At: t0}}
	n, err := s.AddWatchedIfMissing(1, ws)
	if err != nil || n != 1 {
		t.Fatalf("added %d (%v), want 1", n, err)
	}
	if sd, _ := seedFor(t, s, 1, known); sd.Watched {
		t.Error("backfill marked an already-known title as watched")
	}
	if n, _ := s.AddWatchedIfMissing(1, ws); n != 0 {
		t.Errorf("second run added %d, want 0", n)
	}
}

func TestRatings(t *testing.T) {
	s := newTestStore(t)
	k := Key{Movie, 1}
	if err := s.SetRating(1, k, 1); err != nil {
		t.Fatal(err)
	}
	s.SetRating(1, k, -1)
	if r, _ := s.Ratings(1); r[k] != -1 {
		t.Fatalf("rating %d, want -1 after changing it", r[k])
	}
	s.SetRating(1, k, 0)
	if r, _ := s.Ratings(1); len(r) != 0 {
		t.Fatalf("ratings %v, want none after clearing", r)
	}
	for _, bad := range []int{2, -2, 5} {
		if err := s.SetRating(1, k, bad); err == nil {
			t.Errorf("rating %d accepted", bad)
		}
	}
	if r, _ := s.Ratings(1); len(r) != 0 {
		t.Fatalf("a refused rating was stored: %v", r)
	}
}

func TestBlocks(t *testing.T) {
	s := newTestStore(t)
	s.Block(1, Title{Type: Movie, ID: 1, Title: "One"})
	s.Block(1, Title{Type: Movie, ID: 1, Title: "One again"})
	s.Block(2, Title{Type: TV, ID: 9, Title: "Theirs"})
	bs, _ := s.Blocks(1)
	if len(bs) != 1 || bs[0].Title.Title != "One" {
		t.Fatalf("blocks %+v, want only One once", bs)
	}
	s.Unblock(1, Key{Movie, 1})
	if bs, _ := s.Blocks(1); len(bs) != 0 {
		t.Fatalf("still blocked after unblock: %+v", bs)
	}
	if bs, _ := s.Blocks(2); len(bs) != 1 {
		t.Fatal("unblocking for user 1 touched user 2")
	}
}

func TestHasWatched(t *testing.T) {
	s := newTestStore(t)
	s.AddRequested(1, Key{Movie, 1}, "Requested only", t0)
	s.AddWatched(1, []Watched{{Key: Key{Movie, 2}, At: t0}})
	for k, want := range map[Key]bool{{Movie, 1}: false, {Movie, 2}: true, {Movie, 3}: false, {TV, 2}: false} {
		if got, _ := s.HasWatched(1, k); got != want {
			t.Errorf("HasWatched(%v) = %v, want %v", k, got, want)
		}
	}
}
