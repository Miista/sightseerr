package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver: the binary stays CGO_ENABLED=0
)

// Store is sightseerr's own memory: what each person watched and requested (kept
// after Jellyfin and Seerr forget it, which a reap makes them do), their
// ratings and blocks, and the trails they followed. A person is their Seerr
// user ID throughout.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS seeds (
	user         INTEGER NOT NULL,
	type         TEXT    NOT NULL,
	tmdb         INTEGER NOT NULL,
	title        TEXT    NOT NULL DEFAULT '',
	watched_at   INTEGER,          -- unix seconds; NULL = not watched
	requested_at INTEGER,          -- unix seconds; NULL = not requested
	PRIMARY KEY (user, type, tmdb)
);
CREATE TABLE IF NOT EXISTS ratings (
	user  INTEGER NOT NULL,
	type  TEXT    NOT NULL,
	tmdb  INTEGER NOT NULL,
	value INTEGER NOT NULL CHECK (value IN (-1, 1)),
	at    INTEGER NOT NULL,
	PRIMARY KEY (user, type, tmdb)
);
CREATE TABLE IF NOT EXISTS blocks (
	user   INTEGER NOT NULL,
	type   TEXT    NOT NULL,
	tmdb   INTEGER NOT NULL,
	title  TEXT    NOT NULL DEFAULT '',
	year   TEXT    NOT NULL DEFAULT '',
	poster TEXT    NOT NULL DEFAULT '',
	at     INTEGER NOT NULL,
	PRIMARY KEY (user, type, tmdb)
);
CREATE TABLE IF NOT EXISTS edges (
	user INTEGER NOT NULL,
	src  TEXT    NOT NULL,   -- "movie:603"
	dst  TEXT    NOT NULL,
	at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS edges_user_dst ON edges (user, dst);
CREATE TABLE IF NOT EXISTS request_trails (
	user  INTEGER NOT NULL,
	type  TEXT    NOT NULL,
	tmdb  INTEGER NOT NULL,
	trail TEXT    NOT NULL,  -- "movie:603,tv:1399,...", seed first
	at    INTEGER NOT NULL
);
`

// storePath is the database in the data folder. Before the rename the app was
// called spor and kept spor.db: if that is all there is, it is renamed once,
// its -wal and -shm first, so a stop halfway leaves nothing behind (the
// database is moved last and joins its already moved WAL). An existing
// sightseerr.db always wins and spor.db is left alone.
func storePath(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "sightseerr.db")
	old := filepath.Join(dataDir, "spor.db")
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return path, err
	}
	if _, err := os.Stat(old); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		return path, err
	}
	for _, suffix := range []string{"-wal", "-shm", ""} {
		if err := os.Rename(old+suffix, path+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return path, fmt.Errorf("cannot move %s to %s: %w", old+suffix, path+suffix, err)
		}
	}
	return path, nil
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are rare and tiny, and it rules out SQLITE_BUSY
	// between connections of this process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot prepare the database at %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Seed is a title a person's recommendations grow from.
type Seed struct {
	Key       Key
	Title     string
	Watched   bool
	Requested bool
	Starred   bool      // on the Seerr watchlist; never stored, read from Seerr per page
	At        time.Time // the latest of watched and requested
}

func unix(t time.Time) int64 { return t.Unix() }

// AddWatched records titles as watched. A title already recorded keeps the
// later of the two dates; nothing is ever removed, so a reaped title stays.
func (s *Store) AddWatched(user int, ws []Watched) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO seeds (user, type, tmdb, title, watched_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user, type, tmdb) DO UPDATE SET
			title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE seeds.title END,
			watched_at = MAX(COALESCE(seeds.watched_at, 0), excluded.watched_at)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, w := range ws {
		at := w.At
		if at.IsZero() {
			at = s.now()
		}
		if _, err := stmt.Exec(user, string(w.Key.Type), w.Key.ID, w.Title, unix(at)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddWatchedIfMissing records watched titles only where sightseerr has nothing on
// the title yet. Used by the one-off backfill, so running it twice, or after
// the live sync, changes nothing. It reports how many it added.
func (s *Store) AddWatchedIfMissing(user int, ws []Watched) (int, error) {
	added := 0
	for _, w := range ws {
		res, err := s.db.Exec(`INSERT INTO seeds (user, type, tmdb, title, watched_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (user, type, tmdb) DO NOTHING`, user, string(w.Key.Type), w.Key.ID, w.Title, unix(w.At))
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

// AddRequested records a request. The first request date is kept.
func (s *Store) AddRequested(user int, k Key, title string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO seeds (user, type, tmdb, title, requested_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user, type, tmdb) DO UPDATE SET
			title = CASE WHEN excluded.title <> '' AND seeds.title = '' THEN excluded.title ELSE seeds.title END,
			requested_at = COALESCE(seeds.requested_at, excluded.requested_at)`,
		user, string(k.Type), k.ID, title, unix(at))
	return err
}

func (s *Store) SetTitle(user int, k Key, title string) error {
	_, err := s.db.Exec(`UPDATE seeds SET title = ? WHERE user = ? AND type = ? AND tmdb = ?`, title, user, string(k.Type), k.ID)
	return err
}

func (s *Store) Seeds(user int) ([]Seed, error) {
	rows, err := s.db.Query(`SELECT type, tmdb, title, watched_at, requested_at FROM seeds WHERE user = ?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seed
	for rows.Next() {
		var t string
		var sd Seed
		var watched, requested sql.NullInt64
		if err := rows.Scan(&t, &sd.Key.ID, &sd.Title, &watched, &requested); err != nil {
			return nil, err
		}
		sd.Key.Type = MediaType(t)
		sd.Watched, sd.Requested = watched.Valid, requested.Valid
		sd.At = time.Unix(max(watched.Int64, requested.Int64), 0)
		out = append(out, sd)
	}
	return out, rows.Err()
}

// HasWatched reports whether the person watched the title, as sightseerr knows it.
func (s *Store) HasWatched(user int, k Key) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM seeds WHERE user = ? AND type = ? AND tmdb = ? AND watched_at IS NOT NULL`,
		user, string(k.Type), k.ID).Scan(&n)
	return n > 0, err
}

// SetRating stores 1 (liked) or -1 (disliked); 0 clears the rating.
func (s *Store) SetRating(user int, k Key, value int) error {
	if value == 0 {
		_, err := s.db.Exec(`DELETE FROM ratings WHERE user = ? AND type = ? AND tmdb = ?`, user, string(k.Type), k.ID)
		return err
	}
	if value != 1 && value != -1 {
		return fmt.Errorf("a rating is 1, -1 or 0, not %d", value)
	}
	_, err := s.db.Exec(`INSERT INTO ratings (user, type, tmdb, value, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user, type, tmdb) DO UPDATE SET value = excluded.value, at = excluded.at`,
		user, string(k.Type), k.ID, value, unix(s.now()))
	return err
}

func (s *Store) Ratings(user int) (map[Key]int, error) {
	rows, err := s.db.Query(`SELECT type, tmdb, value FROM ratings WHERE user = ?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]int{}
	for rows.Next() {
		var t string
		var k Key
		var v int
		if err := rows.Scan(&t, &k.ID, &v); err != nil {
			return nil, err
		}
		k.Type = MediaType(t)
		out[k] = v
	}
	return out, rows.Err()
}

// Blocked is a hidden title, with enough to show it on the Blocked page.
type Blocked struct {
	Title
	At time.Time `json:"at"`
}

func (s *Store) Block(user int, t Title) error {
	_, err := s.db.Exec(`INSERT INTO blocks (user, type, tmdb, title, year, poster, at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user, type, tmdb) DO NOTHING`,
		user, string(t.Type), t.ID, t.Title, t.Year, t.Poster, unix(s.now()))
	return err
}

func (s *Store) Unblock(user int, k Key) error {
	_, err := s.db.Exec(`DELETE FROM blocks WHERE user = ? AND type = ? AND tmdb = ?`, user, string(k.Type), k.ID)
	return err
}

// Blocks is the person's blocked titles, newest first.
func (s *Store) Blocks(user int) ([]Blocked, error) {
	rows, err := s.db.Query(`SELECT type, tmdb, title, year, poster, at FROM blocks WHERE user = ? ORDER BY at DESC, rowid DESC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Blocked{}
	for rows.Next() {
		var t string
		var b Blocked
		var at int64
		if err := rows.Scan(&t, &b.ID, &b.Title.Title, &b.Year, &b.Poster, &at); err != nil {
			return nil, err
		}
		b.Type, b.At = MediaType(t), time.Unix(at, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddEdge records one step of exploration: from one title's page to another.
func (s *Store) AddEdge(user int, from, to Key) error {
	_, err := s.db.Exec(`INSERT INTO edges (user, src, dst, at) VALUES (?, ?, ?, ?)`,
		user, from.String(), to.String(), unix(s.now()))
	return err
}

// AddRequestTrail stores the trail that led to a request, seed first.
func (s *Store) AddRequestTrail(user int, k Key, trail []Key) error {
	parts := make([]string, len(trail))
	for i, t := range trail {
		parts[i] = t.String()
	}
	_, err := s.db.Exec(`INSERT INTO request_trails (user, type, tmdb, trail, at) VALUES (?, ?, ?, ?, ?)`,
		user, string(k.Type), k.ID, strings.Join(parts, ","), unix(s.now()))
	return err
}
