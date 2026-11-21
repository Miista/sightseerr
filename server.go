package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/miista/authy"
	"github.com/rs/zerolog"
)

// seerrAPI is what the pages need from Seerr: cachedSeerr in production, a
// fake in tests.
type seerrAPI interface {
	Details(ctx context.Context, k Key) (Details, error)
	Recommendations(ctx context.Context, k Key) ([]Title, error)
	Request(ctx context.Context, asUser int, k Key, ch Choice) error
	RequestOptions(ctx context.Context, mt MediaType) (RequestOptions, error)
	CollectionParts(ctx context.Context, id int) ([]Title, error)
}

// Server is the HTTP API and the page.
type Server struct {
	seerr    seerrAPI
	store    *Store
	people   *directory
	statuses *statusBook
	ready    func() bool // the first sync has finished
	static   fs.FS
	log      zerolog.Logger
	now      func() time.Time
	// anonymous is the Seerr user everyone acts as when the login is off
	// (SPOR_AUTH_DISABLED, local trials only). 0 means nobody: with no login
	// and no SPOR_ANONYMOUS_USER, every page refuses.
	anonymous int
	// seerrPublic is Seerr's address as a browser reaches it, for "Open in
	// Seerr" links (SPOR_SEERR_PUBLIC_URL, else SPOR_SEERR_URL). Empty: no links.
	seerrPublic string
	// dryRun (SPOR_REQUESTS_DRY_RUN) logs a request instead of sending it to
	// Seerr, for trying spor against real data without downloading anything.
	// Everything else behaves as if the request was made.
	dryRun bool
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/wall", s.wall)
	mux.HandleFunc("GET /api/title/{type}/{id}", s.title)
	mux.HandleFunc("POST /api/request", s.request)
	mux.HandleFunc("GET /api/request-options/{type}", s.requestOptions)
	mux.HandleFunc("GET /api/library", s.library)
	mux.HandleFunc("POST /api/rating", s.rating)
	mux.HandleFunc("GET /api/blocked", s.blocked)
	mux.HandleFunc("POST /api/block", s.block)
	mux.HandleFunc("POST /api/unblock", s.unblock)
	// The page's own routes all load the one page; app.js reads the path.
	for _, p := range []string{"GET /movie/{id}", "GET /tv/{id}", "GET /library", "GET /blocked"} {
		mux.HandleFunc(p, s.page)
	}
	mux.Handle("GET /", revalidate(http.FileServerFS(s.static)))
	return s.logRequests(sameSiteOnly(mux))
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.Error(w, "page missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// userFrom is authy.UserFrom; tests replace it, since only the gate can put a
// user on a request.
var userFrom = authy.UserFrom

// errNotLinked is a signed-in person spor can't tie to a Seerr account.
var errNotLinked = errors.New("not linked")

// person is the Seerr account the request acts for. It comes only from the
// session (or the operator's SPOR_ANONYMOUS_USER with the login off), never
// from anything the page sends: it decides whose name goes on a request.
func (s *Server) person(r *http.Request) (SeerrUser, error) {
	u, ok := userFrom(r)
	if !ok {
		if s.anonymous > 0 {
			if su, ok := s.people.byID(s.anonymous); ok {
				return su, nil
			}
			return SeerrUser{}, fmt.Errorf("%w: Seerr user %d (SPOR_ANONYMOUS_USER) does not exist", errNotLinked, s.anonymous)
		}
		return SeerrUser{}, errors.New("signed out")
	}
	su, ok := s.people.resolve(u.Email)
	if !ok {
		return SeerrUser{}, fmt.Errorf("%w: no Seerr account has the email you signed in with", errNotLinked)
	}
	return su, nil
}

// who is person for handlers: it answers the request itself when there is
// nobody to act for.
func (s *Server) who(w http.ResponseWriter, r *http.Request) (SeerrUser, bool) {
	su, err := s.person(r)
	switch {
	case err == nil:
		return su, true
	case errors.Is(err, errNotLinked):
		writeError(w, http.StatusForbidden, err)
	default:
		writeError(w, http.StatusUnauthorized, err)
	}
	return SeerrUser{}, false
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	su, err := s.person(r)
	if err != nil {
		reason := strings.TrimPrefix(err.Error(), errNotLinked.Error()+": ")
		writeJSON(w, http.StatusOK, map[string]any{"linked": false, "reason": userMessage(errors.New(reason)), "name": userName(r)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": true, "name": nameOf(su), "jellyfin": su.JellyfinUserID != ""})
}

// exclusions gathers what must not be offered to the person.
func (s *Server) exclusions(user int) (Exclusions, []Seed, map[Key]int, error) {
	seeds, err := s.store.Seeds(user)
	if err != nil {
		return Exclusions{}, nil, nil, err
	}
	ratings, err := s.store.Ratings(user)
	if err != nil {
		return Exclusions{}, nil, nil, err
	}
	blocks, err := s.store.Blocks(user)
	if err != nil {
		return Exclusions{}, nil, nil, err
	}
	ex := Exclusions{statuses: s.statuses.snapshot(), own: map[Key]bool{}, blocked: map[Key]bool{}}
	for _, sd := range seeds {
		ex.own[sd.Key] = true
	}
	for _, b := range blocks {
		ex.blocked[b.Key()] = true
	}
	return ex, seeds, ratings, nil
}

func (s *Server) wall(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	if !s.ready() {
		writeJSON(w, http.StatusOK, map[string]any{"syncing": true, "cards": []Card{}})
		return
	}
	ex, seeds, ratings, err := s.exclusions(su.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cards, failed := buildWall(r.Context(), s.seerr, seeds, ratings, ex)
	if failed > 0 {
		s.log.Warn().Msg(fmt.Sprintf("the wall for %s is missing the recommendations of %d title(s) Seerr didn't answer for", nameOf(su), failed))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": cards, "seeds": len(seeds), "failed": failed, "blocked": len(ex.blocked)})
}

// maxTrail caps how much of a trail is kept: the URL carries it, so it is
// whatever someone typed.
const maxTrail = 12

// parseTrail reads "movie:603,tv:1399" from ?via=, dropping anything malformed
// and keeping the last maxTrail steps.
func parseTrail(s string) []Key {
	var out []Key
	for _, part := range strings.Split(s, ",") {
		if k, ok := parseKey(strings.TrimSpace(part)); ok {
			out = append(out, k)
		}
	}
	if len(out) > maxTrail {
		out = out[len(out)-maxTrail:]
	}
	return out
}

func pathKey(r *http.Request) (Key, bool) {
	mt, ok := parseMediaType(r.PathValue("type"))
	if !ok {
		return Key{}, false
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		return Key{}, false
	}
	return Key{mt, id}, true
}

// Step is one breadcrumb.
type Step struct {
	Type  MediaType `json:"type"`
	ID    int       `json:"id"`
	Title string    `json:"title"`
}

func (s *Server) title(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	k, ok := pathKey(r)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such title"))
		return
	}
	trail := parseTrail(r.URL.Query().Get("via"))
	ctx := r.Context()

	var (
		wg      sync.WaitGroup
		d       Details
		dErr    error
		recs    []Title
		recsErr error
		steps   = make([]Step, len(trail))
	)
	wg.Add(2)
	go func() { defer wg.Done(); d, dErr = s.seerr.Details(ctx, k) }()
	go func() { defer wg.Done(); recs, recsErr = s.seerr.Recommendations(ctx, k) }()
	for i, t := range trail {
		wg.Add(1)
		go func() {
			defer wg.Done()
			steps[i] = Step{Type: t.Type, ID: t.ID}
			if td, err := s.seerr.Details(ctx, t); err == nil {
				steps[i].Title = td.Title.Title
			}
		}()
	}
	wg.Wait()
	if dErr != nil {
		writeError(w, http.StatusBadGateway, dErr)
		return
	}
	if recsErr != nil {
		s.log.Warn().Msg(fmt.Sprintf("no recommendations for %s: %v", k, recsErr))
	}

	ex, seeds, ratings, err := s.exclusions(su.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The synced status is the truth once there is one; the one in the
	// (cached) details only stands in before the first sync.
	if s.ready() {
		d.Status = ex.statuses[k]
	}
	var watched, requested bool
	names := map[Key]string{}
	for _, sd := range seeds {
		if sd.Key == k {
			watched, requested = sd.Watched, sd.Requested
		}
		names[sd.Key] = sd.Title
	}
	// A step Seerr couldn't name is often the seed itself, which spor knows.
	for i := range steps {
		if steps[i].Title == "" {
			steps[i].Title = names[Key{steps[i].Type, steps[i].ID}]
		}
	}
	ex.own[k] = true // never recommend the page's own title
	if len(trail) > 0 {
		if err := s.store.AddEdge(su.ID, trail[len(trail)-1], k); err != nil {
			s.log.Warn().Msg(fmt.Sprintf("could not record the step to %s: %v", k, err))
		}
	}
	links := map[string]string{}
	if s.seerrPublic != "" {
		links["seerr"] = fmt.Sprintf("%s/%s/%d", s.seerrPublic, k.Type, k.ID)
	}
	var parts []collectionPart
	if d.Collection != nil {
		ts, err := s.seerr.CollectionParts(ctx, d.Collection.ID)
		if err != nil {
			s.log.Warn().Msg(fmt.Sprintf("no parts for collection %d: %v", d.Collection.ID, err))
		}
		seedOf := map[Key]Seed{}
		for _, sd := range seeds {
			seedOf[sd.Key] = sd
		}
		for _, t := range ts {
			if s.ready() {
				t.Status = ex.statuses[t.Key()]
			}
			parts = append(parts, collectionPart{Title: t, State: partState(t, seedOf[t.Key()]), Current: t.Key() == k})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"title": d, "watched": watched, "requested": requested, "rating": ratings[k],
		"requestable":     !inLibrary(d.Status) && !requested,
		"recommendations": ex.filter(recs), "trail": steps, "links": links, "collection": parts,
	})
}

// collectionPart is one movie of the page's collection. Unlike everywhere
// else, parts the person has or has seen are shown too: it's the whole
// series, with where each part stands.
type collectionPart struct {
	Title
	State   string `json:"state"` // "watched", "library", "requested" or "" (new to them)
	Current bool   `json:"current"`
}

// partState says where a part stands for the person; seeing it beats having
// it, which beats having asked for it.
func partState(t Title, sd Seed) string {
	switch {
	case sd.Watched:
		return "watched"
	case t.Status == statusAvailable || t.Status == statusPartial:
		return "library"
	case t.Status == statusPending || t.Status == statusProcessing || sd.Requested:
		return "requested"
	}
	return ""
}

func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	var body struct {
		Type       string   `json:"type"`
		ID         int      `json:"id"`
		Via        []string `json:"via"`
		ProfileID  int      `json:"profileId"`
		RootFolder string   `json:"rootFolder"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	mt, ok := parseMediaType(body.Type)
	if !ok || body.ID <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("no such title"))
		return
	}
	k := Key{mt, body.ID}
	if inLibrary(s.statuses.get(k)) {
		writeError(w, http.StatusConflict, errors.New("this is already in the library or on its way"))
		return
	}
	choice, status, err := s.checkChoice(r.Context(), su, mt, Choice{ProfileID: body.ProfileID, RootFolder: body.RootFolder})
	if err != nil {
		writeError(w, status, err)
		return
	}
	if s.dryRun {
		// Nothing was requested, so nothing is remembered as the person's: no
		// seed, no trail. The page still shows it as requested, and the title
		// stays hidden until the next sync replaces the status book.
		s.log.Warn().Msg(fmt.Sprintf("dry run: would request %s in Seerr as %s (user %d)%s", k, nameOf(su), su.ID, describeChoice(choice)))
		s.statuses.mark(k, statusPending)
		writeJSON(w, http.StatusOK, map[string]any{"status": statusPending, "dryRun": true})
		return
	}
	if err := s.seerr.Request(r.Context(), su.ID, k, choice); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.statuses.mark(k, statusPending)
	name := ""
	if d, err := s.seerr.Details(r.Context(), k); err == nil {
		name = d.Title.Title
	}
	if err := s.store.AddRequested(su.ID, k, name, s.now()); err != nil {
		s.log.Warn().Msg(fmt.Sprintf("requested %s for %s, but could not remember it: %v", k, nameOf(su), err))
	}
	trail := parseTrail(strings.Join(body.Via, ","))
	if err := s.store.AddRequestTrail(su.ID, k, trail); err != nil {
		s.log.Warn().Msg(fmt.Sprintf("could not store the trail to %s: %v", k, err))
	}
	s.log.Info().Msg(fmt.Sprintf("%s requested %q (%s) via %d step(s)%s", nameOf(su), name, k, len(trail), describeChoice(choice)))
	writeJSON(w, http.StatusOK, map[string]any{"status": statusPending, "dryRun": false})
}

// checkChoice vets an advanced request's choice. Only people Seerr lets
// choose may send one, and only a profile or folder the server offers. A
// field left at the server's default is dropped, so Seerr decides as it
// would for a plain request (an override rule, if any, still applies).
func (s *Server) checkChoice(ctx context.Context, su SeerrUser, mt MediaType, ch Choice) (Choice, int, error) {
	if ch == (Choice{}) {
		return ch, 0, nil
	}
	if !su.advanced() {
		return Choice{}, http.StatusForbidden, errors.New("choosing quality or folder needs Advanced Requests in Seerr")
	}
	opts, err := s.seerr.RequestOptions(ctx, mt)
	if err != nil {
		return Choice{}, http.StatusBadGateway, err
	}
	if ch.ProfileID != 0 && !opts.hasProfile(ch.ProfileID) {
		return Choice{}, http.StatusBadRequest, errors.New("that quality profile isn't offered")
	}
	if ch.RootFolder != "" && !opts.hasFolder(ch.RootFolder) {
		return Choice{}, http.StatusBadRequest, errors.New("that folder isn't offered")
	}
	if ch.ProfileID == opts.DefaultProfile {
		ch.ProfileID = 0
	}
	if ch.RootFolder == opts.DefaultFolder {
		ch.RootFolder = ""
	}
	return ch, 0, nil
}

func describeChoice(ch Choice) string {
	var parts []string
	if ch.ProfileID > 0 {
		parts = append(parts, fmt.Sprintf("profile %d", ch.ProfileID))
	}
	if ch.RootFolder != "" {
		parts = append(parts, "folder "+ch.RootFolder)
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, " and ")
}

// requestOptions tells the page whether the person may choose, and from what.
func (s *Server) requestOptions(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	mt, ok := parseMediaType(r.PathValue("type"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such media type"))
		return
	}
	if !su.advanced() {
		writeJSON(w, http.StatusOK, map[string]any{"advanced": false})
		return
	}
	opts, err := s.seerr.RequestOptions(r.Context(), mt)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"advanced": true, "options": opts})
}

// LibraryItem as the Library page shows it.
type libraryCard struct {
	Title
	Watched bool `json:"watched"`
	Rating  int  `json:"rating"`
}

const libraryFetchers = 6

func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	if !s.ready() {
		writeJSON(w, http.StatusOK, map[string]any{"syncing": true, "items": []libraryCard{}})
		return
	}
	seeds, err := s.store.Seeds(su.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ratings, err := s.store.Ratings(su.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	watched := map[Key]bool{}
	for _, sd := range seeds {
		if sd.Watched {
			watched[sd.Key] = true
		}
	}
	keys := s.statuses.libraryKeys()
	inLib := make(map[Key]bool, len(keys))
	for _, k := range keys {
		inLib[k] = true
	}
	// Watched titles no longer in the library (reaped, or backfilled from
	// before spor), newest watch first: they can be rated too.
	var gone []Seed
	for _, sd := range seeds {
		if sd.Watched && !inLib[sd.Key] {
			gone = append(gone, sd)
		}
	}
	slices.SortStableFunc(gone, func(a, b Seed) int { return b.At.Compare(a.At) })
	goneKeys := make([]Key, len(gone))
	for i, sd := range gone {
		goneKeys[i] = sd.Key
	}
	details := s.detailsFor(r.Context(), append(slices.Clone(keys), goneKeys...))

	out := make([]libraryCard, 0, len(keys))
	for _, k := range keys {
		if d, ok := details[k]; ok {
			out = append(out, libraryCard{Title: d.Title, Watched: watched[k], Rating: ratings[k]})
		}
	}
	// What can be rated first; unwatched titles go last, each group keeping
	// Seerr's newest-first order.
	slices.SortStableFunc(out, func(a, b libraryCard) int {
		switch {
		case a.Watched == b.Watched:
			return 0
		case a.Watched:
			return -1
		}
		return 1
	})
	// A gone title Seerr can't describe still shows, by the name spor has:
	// it was watched, so it can still be rated.
	earlier := make([]libraryCard, 0, len(gone))
	for _, sd := range gone {
		t := Title{Type: sd.Key.Type, ID: sd.Key.ID, Title: sd.Title}
		if d, ok := details[sd.Key]; ok {
			t = d.Title
		}
		earlier = append(earlier, libraryCard{Title: t, Watched: true, Rating: ratings[sd.Key]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "earlier": earlier, "missing": len(keys) - len(out)})
}

// detailsFor looks the titles up in Seerr, a few at a time; titles Seerr
// can't describe are left out of the result.
func (s *Server) detailsFor(ctx context.Context, keys []Key) map[Key]Details {
	var mu sync.Mutex
	out := make(map[Key]Details, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, libraryFetchers)
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if d, err := s.seerr.Details(ctx, k); err == nil {
				mu.Lock()
				out[k] = d
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) rating(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	var body struct {
		Type  string `json:"type"`
		ID    int    `json:"id"`
		Value int    `json:"value"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	mt, ok := parseMediaType(body.Type)
	if !ok || body.ID <= 0 || body.Value < -1 || body.Value > 1 {
		writeError(w, http.StatusBadRequest, errors.New("a rating is up, down or none, for one title"))
		return
	}
	k := Key{mt, body.ID}
	seen, err := s.store.HasWatched(su.ID, k)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !seen {
		writeError(w, http.StatusConflict, errors.New("you can only rate what you have watched"))
		return
	}
	if err := s.store.SetRating(su.ID, k, body.Value); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rating": body.Value})
}

func (s *Server) blocked(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	bs, err := s.store.Blocks(su.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": bs})
}

func (s *Server) readKey(w http.ResponseWriter, r *http.Request) (Key, bool) {
	var body struct {
		Type string `json:"type"`
		ID   int    `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return Key{}, false
	}
	mt, ok := parseMediaType(body.Type)
	if !ok || body.ID <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("no such title"))
		return Key{}, false
	}
	return Key{mt, body.ID}, true
}

func (s *Server) block(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	k, ok := s.readKey(w, r)
	if !ok {
		return
	}
	// What the Blocked page shows comes from Seerr, not from the page. If
	// Seerr can't say, the title is still blocked, just shown bare.
	t := Title{Type: k.Type, ID: k.ID}
	if d, err := s.seerr.Details(r.Context(), k); err == nil {
		t = d.Title
	}
	if err := s.store.Block(su.ID, t); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": true})
}

func (s *Server) unblock(w http.ResponseWriter, r *http.Request) {
	su, ok := s.who(w, r)
	if !ok {
		return
	}
	k, ok := s.readKey(w, r)
	if !ok {
		return
	}
	if err := s.store.Unblock(su.ID, k); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": false})
}

// userName is who is signed in, for the log: authy's display name, else the
// username. authy falls back to the subject when a provider sends no names,
// and that is an opaque id, never shown.
func userName(r *http.Request) string {
	u, ok := userFrom(r)
	switch {
	case !ok:
		return "someone"
	case u.DisplayName != "" && u.DisplayName != u.Subject:
		return u.DisplayName
	case u.Username != "":
		return u.Username
	}
	return "someone"
}

// revalidate makes browsers ask for the page's files again on every load. The
// embedded files carry no modification time, so without it a browser can keep an
// old app.js next to a new index.html after an upgrade.
func revalidate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// sameSiteOnly refuses state-changing requests from other sites, and any that
// aren't JSON, so a page elsewhere can't request titles in a signed-in browser.
func sameSiteOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, errors.New("cross-site request refused"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
			writeError(w, http.StatusForbidden, errors.New("cross-site request refused"))
			return
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, errors.New("send JSON"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests writes down every request: method, path (never the query), status,
// time and who. The page's own files and /healthz are debug, the rest info.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		msg := fmt.Sprintf("%s %s answered %d in %s for %s", r.Method, r.URL.Path, sw.status,
			time.Since(start).Round(time.Millisecond), userName(r))
		switch {
		case sw.status >= 500:
			s.log.Error().Msg(msg)
		case sw.status >= 400:
			s.log.Warn().Msg(msg)
		case r.URL.Path == "/healthz" || !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodGet:
			s.log.Debug().Msg(msg)
		default:
			s.log.Info().Msg(msg)
		}
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("the request could not be read"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": userMessage(err)})
}

// userMessage is an error as shown to a person: the "seerr: " prefix is
// dropped, since Seerr's own message says what went wrong.
func userMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), errSeerr.Error()+": ")
	r, size := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[size:]
}
