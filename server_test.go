package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/miista/authy"
	"github.com/rs/zerolog"
)

type requested struct {
	user   int
	key    Key
	choice Choice
}

type fakeSeerr struct {
	details map[Key]Details
	recs    map[Key][]Title
	missing map[Key]bool // Details fails for these
	options RequestOptions
	parts   map[int][]Title // collection ID → parts
	scores  map[Key]Ratings

	mu       sync.Mutex
	requests []requested
}

func (f *fakeSeerr) Details(_ context.Context, k Key) (Details, error) {
	if f.missing[k] {
		return Details{}, errors.New("not found")
	}
	if d, ok := f.details[k]; ok {
		return d, nil
	}
	return Details{Title: Title{Type: k.Type, ID: k.ID, Title: k.String()}}, nil
}

func (f *fakeSeerr) Recommendations(_ context.Context, k Key) ([]Title, error) { return f.recs[k], nil }

func (f *fakeSeerr) Request(_ context.Context, asUser int, k Key, ch Choice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, requested{asUser, k, ch})
	return nil
}

func (f *fakeSeerr) RequestOptions(context.Context, MediaType) (RequestOptions, error) {
	return f.options, nil
}

func (f *fakeSeerr) CollectionParts(_ context.Context, id int) ([]Title, error) {
	return f.parts[id], nil
}

func (f *fakeSeerr) Ratings(_ context.Context, k Key) (Ratings, error) {
	return f.scores[k], nil
}

type harness struct {
	srv   *Server
	seerr *fakeSeerr
	store *Store
}

// newHarness: Seerr users 1 (alice@example.com, admin) and 3 (bob@example.com,
// may request but not choose quality or folder).
func newHarness(t *testing.T) *harness {
	t.Helper()
	st := newTestStore(t)
	people := newDirectory(nil)
	people.set([]SeerrUser{
		{ID: 1, Email: "alice@example.com", DisplayName: "Alice", Permissions: permAdmin},
		{ID: 3, Email: "bob@example.com", DisplayName: "Bob", Permissions: 32},
	})
	fs := &fakeSeerr{details: map[Key]Details{}, recs: map[Key][]Title{}, options: RequestOptions{
		Profiles:       []Profile{{8, "HD-1080p"}, {12, "Ultra-HD"}, {13, "Remux"}},
		RootFolders:    []string{"/media/movies", "/media/movies-4k"},
		DefaultProfile: 8, DefaultFolder: "/media/movies",
	}}
	statuses := &statusBook{}
	statuses.set(nil)
	srv := &Server{
		seerr: fs, store: st, people: people, statuses: statuses, ready: func() bool { return true },
		static: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>spor")}},
		log:    zerolog.Nop(), now: func() time.Time { return t0 },
	}
	return &harness{srv: srv, seerr: fs, store: st}
}

// as makes requests arrive signed in with the given email ("" = signed out).
func as(t *testing.T, email string) {
	t.Helper()
	old := userFrom
	userFrom = func(*http.Request) (authy.User, bool) {
		if email == "" {
			return authy.User{}, false
		}
		return authy.User{Subject: "sub-" + email, Email: email}, true
	}
	t.Cleanup(func() { userFrom = old })
}

func (h *harness) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestRequestIsMadeAsTheSignedInPerson(t *testing.T) {
	h := newHarness(t)
	as(t, "bob@example.com")
	// Fields the page might add to try to request as someone else are refused.
	if w, _ := h.do(t, "POST", "/api/request", `{"type":"movie","id":603,"userId":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", w.Code)
	}
	w, _ := h.do(t, "POST", "/api/request", `{"type":"movie","id":603,"via":["movie:1","tv:2"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(h.seerr.requests) != 1 || h.seerr.requests[0] != (requested{3, Key{Movie, 603}, Choice{}}) {
		t.Fatalf("Seerr got %+v, want one request as user 3", h.seerr.requests)
	}
	// It is remembered as Bob's seed and hidden from now on.
	if sd, ok := seedFor(t, h.store, 3, Key{Movie, 603}); !ok || !sd.Requested {
		t.Errorf("request not stored as a seed: %+v", sd)
	}
	if !inLibrary(h.srv.statuses.get(Key{Movie, 603})) {
		t.Error("requested title not marked pending")
	}
}

func TestAdvancedRequestChoice(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	cases := []struct {
		body string
		code int
		want Choice // what reaches Seerr when accepted
	}{
		{`{"type":"movie","id":1,"profileId":13,"rootFolder":"/media/movies-4k"}`, http.StatusOK, Choice{13, "/media/movies-4k"}},
		// Defaults are dropped: Seerr decides, as for a plain request.
		{`{"type":"movie","id":2,"profileId":8,"rootFolder":"/media/movies"}`, http.StatusOK, Choice{}},
		{`{"type":"movie","id":3,"profileId":12}`, http.StatusOK, Choice{ProfileID: 12}},
		// Anything the server doesn't offer is refused, and nothing is requested.
		{`{"type":"movie","id":4,"profileId":99}`, http.StatusBadRequest, Choice{}},
		{`{"type":"movie","id":5,"rootFolder":"/etc"}`, http.StatusBadRequest, Choice{}},
	}
	for _, c := range cases {
		before := len(h.seerr.requests)
		w, _ := h.do(t, "POST", "/api/request", c.body)
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.body, w.Code, c.code, w.Body)
			continue
		}
		sent := len(h.seerr.requests) - before
		if c.code != http.StatusOK {
			if sent != 0 {
				t.Errorf("%s: refused but Seerr got a request", c.body)
			}
			continue
		}
		if sent != 1 || h.seerr.requests[before].choice != c.want {
			t.Errorf("%s: Seerr got %+v, want choice %+v", c.body, h.seerr.requests[before:], c.want)
		}
	}
}

func TestChoiceRefusedWithoutAdvancedRequests(t *testing.T) {
	h := newHarness(t)
	as(t, "bob@example.com")
	if w, _ := h.do(t, "POST", "/api/request", `{"type":"movie","id":1,"profileId":13}`); w.Code != http.StatusForbidden {
		t.Fatalf("Bob choosing a profile: %d, want 403", w.Code)
	}
	if len(h.seerr.requests) != 0 {
		t.Fatalf("Seerr got %+v", h.seerr.requests)
	}
	// Bob is told there's nothing to choose; Alice gets the options.
	if _, out := h.do(t, "GET", "/api/request-options/movie", ""); out["advanced"] != false || out["options"] != nil {
		t.Errorf("Bob's options: %v", out)
	}
	as(t, "alice@example.com")
	_, out := h.do(t, "GET", "/api/request-options/movie", "")
	opts, _ := out["options"].(map[string]any)
	if out["advanced"] != true || opts["defaultFolder"] != "/media/movies" || len(opts["profiles"].([]any)) != 3 {
		t.Errorf("Alice's options: %v", out)
	}
	if w, _ := h.do(t, "GET", "/api/request-options/person", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown media type: %d", w.Code)
	}
}

func TestDryRunNeverReachesSeerr(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.srv.dryRun = true
	w, out := h.do(t, "POST", "/api/request", `{"type":"movie","id":603}`)
	if w.Code != http.StatusOK || out["dryRun"] != true {
		t.Fatalf("status %d, %v", w.Code, out)
	}
	if len(h.seerr.requests) != 0 {
		t.Fatalf("dry run sent %+v to Seerr", h.seerr.requests)
	}
	// Nothing was requested, so it must not become the person's request: no
	// seed ("because you requested …") and no stored trail.
	if sd, ok := seedFor(t, h.store, 1, Key{Movie, 603}); ok {
		t.Errorf("dry-run request stored as a seed: %+v", sd)
	}
	var n int
	h.store.db.QueryRow(`SELECT COUNT(*) FROM request_trails`).Scan(&n)
	if n != 0 {
		t.Errorf("dry run stored %d trail(s)", n)
	}
	// The title is still hidden for now, as if requested.
	if !inLibrary(h.srv.statuses.get(Key{Movie, 603})) {
		t.Error("dry-run title not hidden")
	}
}

func TestRequestRefusedForTitlesAlreadyHad(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.srv.statuses.set([]LibraryItem{{Key: Key{Movie, 603}, Status: statusAvailable}})
	if w, _ := h.do(t, "POST", "/api/request", `{"type":"movie","id":603}`); w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	if len(h.seerr.requests) != 0 {
		t.Fatalf("Seerr was asked anyway: %+v", h.seerr.requests)
	}
}

func TestNobodyToActForIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		email string
		code  int
	}{{"", http.StatusUnauthorized}, {"stranger@example.com", http.StatusForbidden}} {
		as(t, c.email)
		for _, p := range []string{"/api/wall", "/api/library", "/api/blocked", "/api/title/movie/1"} {
			if w, _ := h.do(t, "GET", p, ""); w.Code != c.code {
				t.Errorf("%q GET %s: %d, want %d", c.email, p, w.Code, c.code)
			}
		}
		if w, _ := h.do(t, "POST", "/api/request", `{"type":"movie","id":1}`); w.Code != c.code {
			t.Errorf("%q request: %d, want %d", c.email, w.Code, c.code)
		}
	}
	if len(h.seerr.requests) != 0 {
		t.Fatalf("Seerr got requests from nobody: %+v", h.seerr.requests)
	}
}

func TestMeSaysWhenNotLinked(t *testing.T) {
	h := newHarness(t)
	as(t, "stranger@example.com")
	_, out := h.do(t, "GET", "/api/me", "")
	if out["linked"] != false || out["reason"] == "" {
		t.Fatalf("me = %v", out)
	}
	as(t, "alice@example.com")
	if _, out := h.do(t, "GET", "/api/me", ""); out["linked"] != true || out["name"] != "Alice" {
		t.Fatalf("me = %v", out)
	}
}

func TestAnonymousOnlyWithTheOperatorsSetting(t *testing.T) {
	h := newHarness(t)
	as(t, "")
	if w, _ := h.do(t, "GET", "/api/wall", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no login and no SPOR_ANONYMOUS_USER: %d, want 401", w.Code)
	}
	h.srv.anonymous = 3
	h.do(t, "POST", "/api/request", `{"type":"movie","id":5}`)
	if len(h.seerr.requests) != 1 || h.seerr.requests[0].user != 3 {
		t.Fatalf("Seerr got %+v, want one request as the configured user 3", h.seerr.requests)
	}
}

func TestTitlePageFiltersAndRecordsTheStep(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	page := Key{Movie, 100}
	h.seerr.details[page] = Details{Title: Title{Type: Movie, ID: 100, Title: "Page", Status: statusAvailable}}
	h.seerr.recs[page] = []Title{mv(100, "Itself"), mv(1, "Watched"), mv(2, "In library"), mv(3, "Blocked"), mv(4, "New")}
	h.store.AddWatched(1, []Watched{{Key: Key{Movie, 1}, At: t0}})
	h.srv.statuses.set([]LibraryItem{{Key: Key{Movie, 2}, Status: statusAvailable}})
	h.store.Block(1, mv(3, "Blocked"))

	w, out := h.do(t, "GET", "/api/title/movie/100?via=movie:7,bogus,tv:8", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	recs := out["recommendations"].([]any)
	if len(recs) != 1 || recs[0].(map[string]any)["title"] != "New" {
		t.Fatalf("recommendations %v, want only New", recs)
	}
	if trail := out["trail"].([]any); len(trail) != 2 {
		t.Errorf("trail %v, want the two valid steps", trail)
	}
	// The synced status (none) wins over the cached details' "available".
	if out["requestable"] != true {
		t.Errorf("requestable = %v, want true", out["requestable"])
	}
	var n int
	h.store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE user = 1 AND src = 'tv:8' AND dst = 'movie:100'`).Scan(&n)
	if n != 1 {
		t.Errorf("edge from the last step not recorded (%d)", n)
	}
	// Another person sees their own exclusions, not Alice's.
	as(t, "bob@example.com")
	if _, out := h.do(t, "GET", "/api/title/movie/100", ""); len(out["recommendations"].([]any)) != 3 {
		t.Errorf("Bob's recommendations %v, want Watched, Blocked and New", out["recommendations"])
	}
}

func TestTitleLinksToSeerr(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.srv.seerrPublic = "https://seerr.example.com"
	h.seerr.details[Key{Movie, 188927}] = Details{Title: Title{Type: Movie, ID: 188927, Title: "Star Trek Beyond"},
		Collection: &Collection{ID: 115575, Name: "Star Trek: Alternate Reality Collection"}}
	_, out := h.do(t, "GET", "/api/title/movie/188927", "")
	links := out["links"].(map[string]any)
	if links["seerr"] != "https://seerr.example.com/movie/188927" || len(links) != 1 {
		t.Errorf("links %v", links)
	}
	_, out = h.do(t, "GET", "/api/title/tv/1399", "")
	if links := out["links"].(map[string]any); links["seerr"] != "https://seerr.example.com/tv/1399" {
		t.Errorf("show links %v", links)
	}
	h.srv.seerrPublic = ""
	if _, out := h.do(t, "GET", "/api/title/movie/188927", ""); len(out["links"].(map[string]any)) != 0 {
		t.Errorf("links without a public URL: %v", out["links"])
	}
}

func TestCollectionShowsEveryPartWithItsState(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	page := Key{Movie, 3}
	h.seerr.details[page] = Details{Title: Title{Type: Movie, ID: 3, Title: "Beyond"}, Collection: &Collection{ID: 9, Name: "Trek"}}
	h.seerr.parts = map[int][]Title{9: {mv(1, "Trek"), mv(2, "Darkness"), mv(3, "Beyond"), mv(4, "Next"), mv(5, "Asked")}}
	h.store.AddWatched(1, []Watched{{Key: Key{Movie, 1}, At: t0}})
	h.store.AddRequested(1, Key{Movie, 5}, "Asked", t0)
	h.store.Block(1, mv(4, "Next")) // blocked, yet still part of the series
	h.srv.statuses.set([]LibraryItem{{Key: Key{Movie, 1}, Status: statusAvailable}, {Key: Key{Movie, 2}, Status: statusAvailable}})

	_, out := h.do(t, "GET", "/api/title/movie/3", "")
	var got []string
	for _, p := range out["collection"].([]any) {
		m := p.(map[string]any)
		s := fmt.Sprintf("%s:%s", m["title"], m["state"])
		if m["current"] == true {
			s += ":here"
		}
		got = append(got, s)
	}
	want := "Trek:watched Darkness:library Beyond::here Next: Asked:requested"
	if strings.Join(got, " ") != want {
		t.Fatalf("parts %v\nwant %s", got, want)
	}
	// Parts never leak into "More like this" through the collection.
	if _, out := h.do(t, "GET", "/api/title/tv/1", ""); out["collection"] != nil {
		t.Errorf("a show got collection parts: %v", out["collection"])
	}
}

func TestTitlePathAheadSeedAndMoreLikeThis(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	page := Key{Movie, 100}
	h.seerr.details[page] = Details{Title: Title{Type: Movie, ID: 100, Title: "Page"}, Collection: &Collection{ID: 9, Name: "Saga"}}
	h.seerr.parts = map[int][]Title{9: {mv(100, "Page"), mv(1, "Sequel")}}
	var recs []Title
	recs = append(recs, mv(1, "Sequel")) // in the collection: not in "More like this"
	for i := 0; i < 12; i++ {
		recs = append(recs, mv(200+i, fmt.Sprintf("Rec %d", i)))
	}
	h.seerr.recs[page] = recs
	h.seerr.scores = map[Key]Ratings{page: {RTCritics: 86, IMDb: 8.8}}
	h.seerr.details[Key{Movie, 7}] = Details{Title: Title{Type: Movie, ID: 7, Title: "Later one"}}

	_, out := h.do(t, "GET", "/api/title/movie/100?ahead=movie:7&seed=movie:55", "")
	more := out["recommendations"].([]any)
	if len(more) != maxMoreLikeThis {
		t.Errorf("%d recommendations, want %d", len(more), maxMoreLikeThis)
	}
	for _, m := range more {
		if m.(map[string]any)["title"] == "Sequel" {
			t.Error("a collection part is also in More like this")
		}
	}
	if a := out["ahead"].([]any); len(a) != 1 || a[0].(map[string]any)["title"] != "Later one" {
		t.Errorf("ahead = %v", a)
	}
	if s := out["scores"].(map[string]any); s["rtCritics"] != float64(86) || s["imdb"] != 8.8 {
		t.Errorf("scores = %v", s)
	}
	// Straight from Discover (no path yet) the step is recorded from the seed.
	var n int
	h.store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE src = 'movie:55' AND dst = 'movie:100'`).Scan(&n)
	if n != 1 {
		t.Errorf("seed edge recorded %d times, want 1", n)
	}
	// With a path, the step comes from the last title on it, not the seed.
	h.do(t, "GET", "/api/title/movie/100?via=movie:7&seed=movie:55", "")
	h.store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE src = 'movie:55'`).Scan(&n)
	if n != 1 {
		t.Errorf("seed used although the path had a title (%d edges from it)", n)
	}
}

func TestWhyEndpoint(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	seed := Key{Movie, 1}
	h.store.AddWatched(1, []Watched{{Key: seed, Title: "Fall", At: t0}})
	h.seerr.recs[seed] = []Title{mv(5, "Other"), mv(10, "Maze Runner")}
	_, out := h.do(t, "GET", "/api/why/movie/10", "")
	by := out["by"].([]any)
	if out["score"] != 1.0 || len(by) != 1 || out["hidden"] != "" {
		t.Fatalf("why = %v", out)
	}
	if c := by[0].(map[string]any); c["title"] != "Fall" || c["reason"] != "watched" || c["position"] != float64(2) || c["of"] != float64(2) {
		t.Errorf("contribution %v", c)
	}
	// It says why a title isn't on the wall at all.
	h.store.Block(1, mv(10, "Maze Runner"))
	if _, out := h.do(t, "GET", "/api/why/movie/10", ""); out["hidden"] != "blocked" {
		t.Errorf("blocked title: hidden = %v", out["hidden"])
	}
	if _, out := h.do(t, "GET", "/api/why/movie/1", ""); out["hidden"] != "yours" {
		t.Errorf("watched title: hidden = %v", out["hidden"])
	}
	as(t, "")
	if w, _ := h.do(t, "GET", "/api/why/movie/10", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", w.Code)
	}
}

func TestPartState(t *testing.T) {
	cases := []struct {
		status int
		seed   Seed
		want   string
	}{
		{statusAvailable, Seed{Watched: true}, "watched"}, // seen beats had
		{statusAvailable, Seed{}, "library"},
		{statusPartial, Seed{}, "library"},
		{statusProcessing, Seed{}, "requested"},
		{0, Seed{Requested: true}, "requested"}, // reaped, but asked for
		{statusDeleted, Seed{}, ""},
		{statusBlocklisted, Seed{}, ""},
		{0, Seed{}, ""},
	}
	for _, c := range cases {
		if got := partState(Title{Status: c.status}, c.seed); got != c.want {
			t.Errorf("status %d, seed %+v: %q, want %q", c.status, c.seed, got, c.want)
		}
	}
}

func TestTrailNamesFallBackToTheSeed(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	seed, other := Key{Movie, 7}, Key{Movie, 8}
	h.seerr.missing = map[Key]bool{seed: true, other: true}
	h.store.AddWatched(1, []Watched{{Key: seed, Title: "Interstellar", At: t0}})
	_, out := h.do(t, "GET", "/api/title/movie/100?via=movie:7,movie:8", "")
	trail := out["trail"].([]any)
	if got := trail[0].(map[string]any)["title"]; got != "Interstellar" {
		t.Errorf("seed step named %q, want Interstellar from spor's own record", got)
	}
	// A step that is neither named by Seerr nor a seed stays unnamed, not wrong.
	if got := trail[1].(map[string]any)["title"]; got != "" {
		t.Errorf("unknown step named %q, want empty", got)
	}
}

func TestOnlyWatchedTitlesCanBeRated(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	if w, _ := h.do(t, "POST", "/api/rating", `{"type":"movie","id":1,"value":1}`); w.Code != http.StatusConflict {
		t.Fatalf("rating an unwatched title: %d, want 409", w.Code)
	}
	if r, _ := h.store.Ratings(1); len(r) != 0 {
		t.Fatalf("refused rating stored: %v", r)
	}
	h.store.AddWatched(1, []Watched{{Key: Key{Movie, 1}, At: t0}})
	if w, _ := h.do(t, "POST", "/api/rating", `{"type":"movie","id":1,"value":-1}`); w.Code != http.StatusOK {
		t.Fatalf("rating a watched title: %d", w.Code)
	}
	if r, _ := h.store.Ratings(1); r[Key{Movie, 1}] != -1 {
		t.Fatalf("ratings %v", r)
	}
	if w, _ := h.do(t, "POST", "/api/rating", `{"type":"movie","id":1,"value":3}`); w.Code != http.StatusBadRequest {
		t.Fatalf("value 3: %d, want 400", w.Code)
	}
}

func TestLibraryPutsUnwatchedLast(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	// Seerr's order (newest first): A unwatched, B watched, C unwatched, D watched.
	h.srv.statuses.set([]LibraryItem{
		{Key: Key{Movie, 1}, Status: statusAvailable}, {Key: Key{Movie, 2}, Status: statusAvailable},
		{Key: Key{Movie, 3}, Status: statusAvailable}, {Key: Key{Movie, 4}, Status: statusPartial},
	})
	for id, name := range map[int]string{1: "A", 2: "B", 3: "C", 4: "D"} {
		h.seerr.details[Key{Movie, id}] = Details{Title: Title{Type: Movie, ID: id, Title: name}}
	}
	h.store.AddWatched(1, []Watched{{Key: Key{Movie, 2}, At: t0}, {Key: Key{Movie, 4}, At: t0}})
	_, out := h.do(t, "GET", "/api/library", "")
	var got []string
	for _, it := range out["items"].([]any) {
		got = append(got, it.(map[string]any)["title"].(string))
	}
	if strings.Join(got, "") != "BDAC" {
		t.Fatalf("library order %v, want watched B, D then unwatched A, C (each keeping Seerr's order)", got)
	}
}

func TestLibraryListsWatchedTitlesNoLongerInIt(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.srv.statuses.set([]LibraryItem{{Key: Key{Movie, 1}, Status: statusAvailable}})
	h.seerr.details[Key{Movie, 2}] = Details{Title: Title{Type: Movie, ID: 2, Title: "Reaped", Poster: "/r.jpg"}}
	h.seerr.missing = map[Key]bool{{TV, 3}: true}
	h.store.AddWatched(1, []Watched{
		{Key: Key{Movie, 1}, Title: "Still here", At: t0.Add(5 * time.Hour)}, // in the library: not "earlier"
		{Key: Key{Movie, 2}, Title: "Reaped", At: t0.Add(time.Hour)},
		{Key: Key{TV, 3}, Title: "Gone show", At: t0.Add(3 * time.Hour)}, // Seerr can't describe it
	})
	h.store.AddRequested(1, Key{Movie, 4}, "Only requested", t0) // not watched: not "earlier"
	h.store.SetRating(1, Key{Movie, 2}, 1)
	h.store.AddWatched(3, []Watched{{Key: Key{Movie, 5}, Title: "Bob's", At: t0}}) // someone else's

	_, out := h.do(t, "GET", "/api/library", "")
	var got []string
	for _, it := range out["earlier"].([]any) {
		m := it.(map[string]any)
		got = append(got, fmt.Sprintf("%v:%v:%v", m["title"], m["watched"], m["rating"]))
	}
	// Newest watch first; the one Seerr can't describe shows by spor's own name.
	if strings.Join(got, " ") != "Gone show:true:0 Reaped:true:1" {
		t.Fatalf("earlier = %v", got)
	}
	// And a title in that section can be rated: it was watched.
	if w, _ := h.do(t, "POST", "/api/rating", `{"type":"tv","id":3,"value":-1}`); w.Code != http.StatusOK {
		t.Fatalf("rating a reaped title: %d", w.Code)
	}
}

func TestWallWaitsForTheFirstSync(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.srv.ready = func() bool { return false }
	if _, out := h.do(t, "GET", "/api/wall", ""); out["syncing"] != true {
		t.Fatalf("wall = %v, want syncing", out)
	}
}

func TestBlockAndUnblock(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	h.seerr.details[Key{TV, 5}] = Details{Title: Title{Type: TV, ID: 5, Title: "From Seerr"}}
	h.do(t, "POST", "/api/block", `{"type":"tv","id":5}`)
	_, out := h.do(t, "GET", "/api/blocked", "")
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != "From Seerr" {
		t.Fatalf("blocked %v", items)
	}
	h.do(t, "POST", "/api/unblock", `{"type":"tv","id":5}`)
	if _, out := h.do(t, "GET", "/api/blocked", ""); len(out["items"].([]any)) != 0 {
		t.Fatalf("still blocked: %v", out["items"])
	}
}

func TestCrossSiteWritesRefused(t *testing.T) {
	h := newHarness(t)
	as(t, "alice@example.com")
	r := httptest.NewRequest("POST", "/api/request", strings.NewReader(`{"type":"movie","id":1}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(h.seerr.requests) != 0 {
		t.Fatalf("cross-site request: %d, Seerr got %v", w.Code, h.seerr.requests)
	}
	// A form post (not JSON) is refused too.
	r = httptest.NewRequest("POST", "/api/request", strings.NewReader(`type=movie&id=1`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d, want 415", w.Code)
	}
}

func TestPageRoutesServeThePage(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/movie/603", "/tv/1399", "/library", "/blocked"} {
		w, _ := h.do(t, "GET", p, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "spor") {
			t.Errorf("GET %s: %d %q", p, w.Code, w.Body.String())
		}
	}
	as(t, "alice@example.com")
	if w, _ := h.do(t, "GET", "/api/title/person/5", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown media type: %d, want 404", w.Code)
	}
}

func TestParseTrail(t *testing.T) {
	if got := parseTrail(""); len(got) != 0 {
		t.Errorf("empty trail = %v", got)
	}
	var parts []string
	for i := 1; i <= maxTrail+3; i++ {
		parts = append(parts, fmt.Sprintf("movie:%d", i))
	}
	got := parseTrail(strings.Join(parts, ","))
	if len(got) != maxTrail {
		t.Fatalf("%d steps kept, want %d", len(got), maxTrail)
	}
	if got[len(got)-1].String() != parts[len(parts)-1] {
		t.Errorf("last step %v, want %s (the newest steps are kept)", got[len(got)-1], parts[len(parts)-1])
	}
}
