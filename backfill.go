package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The one-off backfill (`spor backfill <copy of jellyfin.db>`) recovers what
// was watched and reaped before spor's first sync. Jellyfin keeps the watch
// data of a deleted item on a PLACEHOLDER row, keyed by the item's provider
// IDs: a movie leaves rows keyed by its TMDb ID and by its IMDb ID (and one by
// a GUID), all with the same user and last-played time. Jellyfin's API doesn't
// expose these rows, so this reads a copy of the database file. It is never an
// ongoing dependency: run it once, then delete the copy.
//
// A TMDb ID doesn't say whether it is a movie or a show, so each one is looked
// up in Seerr both ways and kept only where the IMDb ID matches.
//
// An episode is keyed differently: by its series' TVDB ID and IMDb ID, each
// with the season and episode appended, three digits apiece
// ("383491001003" and "tt11640020001003" for series 383491, 1x03). Those
// are resolved to the series, through Seerr's search by tvdb: and imdb:, and
// kept only when both lookups find the same show. The show then counts as
// watched, as the live sync does for a played episode.
//
// Rows with only a GUID can't be resolved and are skipped. Only titles spor
// knows nothing about are added, so running it twice changes nothing.

// placeholderWatch is one deleted item's watch data for one person.
type placeholderWatch struct {
	jellyfinUser string
	num          string // the numeric key: a movie's TMDb ID, or an episode's TVDB+SSS+EEE
	imdb         string
	at           time.Time
}

// episodeKeys reads an episode's keys back to its series' TVDB and IMDb IDs.
// It's false unless both keys end in the same six season-and-episode digits.
func episodeKeys(num, imdb string) (tvdb, seriesIMDb string, ok bool) {
	const suffix = 6
	if len(num) <= suffix || len(imdb) <= len("tt")+suffix || num[len(num)-suffix:] != imdb[len(imdb)-suffix:] {
		return "", "", false
	}
	return num[:len(num)-suffix], imdb[:len(imdb)-suffix], true
}

// readPlaceholders groups the played PLACEHOLDER rows by person and last
// played time, and keeps the groups that carry both a TMDb and an IMDb ID.
func readPlaceholders(db *sql.DB) ([]placeholderWatch, int, error) {
	rows, err := db.Query(`SELECT u.UserId, COALESCE(u.LastPlayedDate, ''), u.CustomDataKey
		FROM UserData u JOIN BaseItems b ON b.Id = u.ItemId
		WHERE b.Type = 'PLACEHOLDER' AND u.Played = 1`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type group struct {
		num  string
		imdb string
	}
	groups := map[[2]string]*group{}
	var order [][2]string
	for rows.Next() {
		var user, played, key string
		if err := rows.Scan(&user, &played, &key); err != nil {
			return nil, 0, err
		}
		gk := [2]string{normalizeJellyfinID(user), played}
		g, ok := groups[gk]
		if !ok {
			g = &group{}
			groups[gk] = g
			order = append(order, gk)
		}
		if n, err := strconv.ParseInt(key, 10, 64); err == nil && n > 0 {
			g.num = key
		} else if strings.HasPrefix(key, "tt") {
			g.imdb = key
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var out []placeholderWatch
	skipped := 0
	for _, gk := range order {
		g := groups[gk]
		if g.num == "" || g.imdb == "" {
			skipped++
			continue
		}
		at, _ := time.Parse("2006-01-02 15:04:05.9999999", gk[1])
		out = append(out, placeholderWatch{jellyfinUser: gk[0], num: g.num, imdb: g.imdb, at: at})
	}
	return out, skipped, nil
}

// normalizeJellyfinID makes Jellyfin's two GUID spellings (with and without
// dashes, any case) comparable: the database and Seerr don't agree.
func normalizeJellyfinID(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}

// backfillSeerr is the Seerr calls the backfill needs.
type backfillSeerr interface {
	Details(ctx context.Context, k Key) (Details, error)
	Search(ctx context.Context, query string) ([]Title, error)
}

// resolvePlaceholder finds which title a deleted item was: a movie (or show)
// by its TMDb ID, confirmed by its IMDb ID; failing that, the series of an
// episode, found by its series' TVDB and IMDb IDs, which must agree.
func resolvePlaceholder(ctx context.Context, seerr backfillSeerr, p placeholderWatch) (Title, bool) {
	// TMDb IDs are well under 100 million; an episode's 12-digit key isn't one.
	if id, err := strconv.Atoi(p.num); err == nil && id > 0 && id < 100_000_000 {
		for _, mt := range []MediaType{Movie, TV} {
			d, err := seerr.Details(ctx, Key{mt, id})
			if err == nil && strings.EqualFold(d.IMDb, p.imdb) {
				return d.Title, true
			}
		}
	}
	tvdb, imdb, ok := episodeKeys(p.num, p.imdb)
	if !ok {
		return Title{}, false
	}
	byTVDB, ok1 := firstShow(ctx, seerr, "tvdb:"+tvdb)
	byIMDb, ok2 := firstShow(ctx, seerr, "imdb:"+imdb)
	if !ok1 || !ok2 || byTVDB.ID != byIMDb.ID {
		return Title{}, false
	}
	return byTVDB, true
}

// firstShow is the first TV result of a Seerr search.
func firstShow(ctx context.Context, seerr backfillSeerr, query string) (Title, bool) {
	ts, err := seerr.Search(ctx, query)
	if err != nil {
		return Title{}, false
	}
	for _, t := range ts {
		if t.Type == TV {
			return t, true
		}
	}
	return Title{}, false
}

func runBackfill(ctx context.Context, dbPath string, getenv func(string) string, out io.Writer) error {
	seerrURL, seerrKey := getenv("SPOR_SEERR_URL"), getenv("SPOR_SEERR_API_KEY")
	if seerrURL == "" || seerrKey == "" {
		return fmt.Errorf("SPOR_SEERR_URL and SPOR_SEERR_API_KEY must be set")
	}
	dataDir := getenv("SPOR_DATA_DIR")
	if dataDir == "" {
		dataDir = defaultDataDir
	}

	jf, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer jf.Close()
	watches, skipped, err := readPlaceholders(jf)
	if err != nil {
		return fmt.Errorf("cannot read %s as a Jellyfin database: %w", dbPath, err)
	}

	seerr := newSeerrClient(seerrURL, seerrKey)
	users, err := seerr.Users(ctx)
	if err != nil {
		return err
	}
	byJellyfin := map[string]SeerrUser{}
	for _, u := range users {
		if u.JellyfinUserID != "" {
			byJellyfin[normalizeJellyfinID(u.JellyfinUserID)] = u
		}
	}

	store, err := openStore(filepath.Join(dataDir, "spor.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	// Many episodes fold into one show, so titles are collected per person
	// first, each with its latest play.
	perUser := map[int]map[Key]Watched{}
	unresolved, noUser := 0, 0
	for _, p := range watches {
		u, ok := byJellyfin[p.jellyfinUser]
		if !ok {
			noUser++
			continue
		}
		t, ok := resolvePlaceholder(ctx, seerr, p)
		if !ok {
			unresolved++
			continue
		}
		if perUser[u.ID] == nil {
			perUser[u.ID] = map[Key]Watched{}
		}
		if w, seen := perUser[u.ID][t.Key()]; !seen || p.at.After(w.At) {
			perUser[u.ID][t.Key()] = Watched{Key: t.Key(), Title: t.Title, At: p.at}
		}
	}
	titles, added := 0, 0
	for user, byKey := range perUser {
		ws := make([]Watched, 0, len(byKey))
		for _, w := range byKey {
			ws = append(ws, w)
		}
		titles += len(ws)
		n, err := store.AddWatchedIfMissing(user, ws)
		if err != nil {
			return err
		}
		added += n
	}
	fmt.Fprintf(out, "read %d deleted items with watch data (movies and episodes): %d titles; added %d new, %d already known; "+
		"%d items not found in Seerr; %d for Jellyfin users with no Seerr account; %d without usable IDs skipped\n",
		len(watches), titles, added, titles-added, unresolved, noUser, skipped)
	return nil
}
