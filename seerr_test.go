package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseKey(t *testing.T) {
	good := map[string]Key{"movie:603": {Movie, 603}, "tv:1399": {TV, 1399}}
	for in, want := range good {
		if got, ok := parseKey(in); !ok || got != want {
			t.Errorf("parseKey(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "movie", "movie:", "movie:0", "movie:-3", "movie:abc", "person:5", "MOVIE:5", "603"} {
		if got, ok := parseKey(in); ok {
			t.Errorf("parseKey(%q) accepted as %v", in, got)
		}
	}
}

func TestInLibrary(t *testing.T) {
	want := map[int]bool{
		0: false, statusUnknown: false, statusDeleted: false,
		statusPending: true, statusProcessing: true, statusPartial: true, statusAvailable: true, statusBlocklisted: true,
	}
	for status, w := range want {
		if got := inLibrary(status); got != w {
			t.Errorf("inLibrary(%d) = %v, want %v", status, got, w)
		}
	}
}

func TestSeerrTitleNormalises(t *testing.T) {
	var movie, show seerrTitle
	json.Unmarshal([]byte(`{"id":1,"mediaType":"movie","title":"Heat","releaseDate":"1995-12-15","posterPath":"/h.jpg","mediaInfo":{"status":5}}`), &movie)
	json.Unmarshal([]byte(`{"id":2,"name":"Dark","firstAirDate":"2017-12-01"}`), &show)
	if got := movie.title(TV); got.Type != Movie || got.Title != "Heat" || got.Year != "1995" || got.Status != 5 {
		t.Errorf("movie normalised to %+v", got)
	}
	// No mediaType (detail pages): the type asked for is used, and shows use name.
	if got := show.title(TV); got.Type != TV || got.Title != "Dark" || got.Year != "2017" || got.Status != 0 {
		t.Errorf("show normalised to %+v", got)
	}
}

func TestSeerrRatings(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/movie/27205/ratingscombined":
			fmt.Fprint(w, `{"rt":{"url":"https://rt/m/inception","criticsRating":"Certified Fresh","criticsScore":86,"audienceRating":"Upright","audienceScore":91},
				"imdb":{"url":"https://imdb/tt1375666","criticsScore":8.8}}`)
		case "/api/v1/movie/1/ratingscombined":
			fmt.Fprint(w, `{"rt":{"criticsRating":"Rotten","criticsScore":20,"audienceRating":"Spilled","audienceScore":30}}`)
		case "/api/v1/tv/1399/ratings":
			fmt.Fprint(w, `{"url":"https://rt/tv/got","criticsRating":"Fresh","criticsScore":89,"audienceRating":"Upright","audienceScore":85}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Rotten Tomatoes ratings not found."}`)
		}
	})
	ctx := context.Background()
	if r, err := c.Ratings(ctx, Key{Movie, 27205}); err != nil || r.RTCritics != 86 || r.RTAudience != 91 || r.IMDb != 8.8 || r.RTCriticsRotten || r.RTAudienceRotten || r.IMDbURL == "" {
		t.Errorf("Inception: %+v, %v", r, err)
	}
	if r, _ := c.Ratings(ctx, Key{Movie, 1}); !r.RTCriticsRotten || !r.RTAudienceRotten || r.IMDb != 0 {
		t.Errorf("rotten movie: %+v", r)
	}
	if r, err := c.Ratings(ctx, Key{TV, 1399}); err != nil || r.RTCritics != 89 || r.RTAudience != 85 || r.IMDb != 0 {
		t.Errorf("show: %+v, %v", r, err)
	}
	// Nothing on Rotten Tomatoes is no ratings, not a failure.
	if r, err := c.Ratings(ctx, Key{Movie, 2}); err != nil || r != (Ratings{}) {
		t.Errorf("no ratings: %+v, %v", r, err)
	}
}

func TestSeerrNotFoundKeepsSeerrsMessage(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Unable to retrieve movie."}`)
	})
	_, err := c.Details(context.Background(), Key{Movie, 5})
	if !errors.Is(err, errSeerr) || !errors.Is(err, errSeerrNotFound) || userMessage(err) != "Unable to retrieve movie." {
		t.Fatalf("error %v shown as %q", err, userMessage(err))
	}
}

func TestDetailsCollection(t *testing.T) {
	var s seerrTitle
	json.Unmarshal([]byte(`{"id":188927,"title":"Star Trek Beyond","collection":{"id":115575,"name":"Star Trek: Alternate Reality Collection"}}`), &s)
	if d := s.details(Movie); d.Collection == nil || d.Collection.ID != 115575 {
		t.Errorf("movie collection %+v", d.Collection)
	}
	var none seerrTitle
	json.Unmarshal([]byte(`{"id":1,"title":"Standalone"}`), &none)
	if d := none.details(Movie); d.Collection != nil {
		t.Errorf("standalone movie got collection %+v", d.Collection)
	}
}

func TestCollectionPartsInReleaseOrder(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/collection/115575" {
			t.Errorf("asked %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"name":"Trek","parts":[
			{"id":188927,"mediaType":"movie","title":"Beyond","releaseDate":"2016-07-07"},
			{"id":999,"mediaType":"movie","title":"Untitled","releaseDate":""},
			{"id":13475,"mediaType":"movie","title":"Star Trek","releaseDate":"2009-05-06","mediaInfo":{"status":5}},
			{"id":54138,"mediaType":"movie","title":"Into Darkness","releaseDate":"2013-05-05"}]}`)
	})
	got, err := c.CollectionParts(context.Background(), 115575)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Title)
	}
	if strings.Join(names, ",") != "Star Trek,Into Darkness,Beyond,Untitled" || got[0].Status != 5 {
		t.Fatalf("parts %v (first status %d), want release order, undated last", names, got[0].Status)
	}
}

// seerrStub records what reached it and answers per path.
type seerrStub struct {
	srv   *httptest.Server
	calls atomic.Int32
	last  *http.Request
	body  map[string]any
}

func newSeerrStub(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*seerrStub, *seerrClient) {
	t.Helper()
	s := &seerrStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.last = r
		s.body = nil
		json.NewDecoder(r.Body).Decode(&s.body)
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s, newSeerrClient(s.srv.URL, "k")
}

func TestRequestActsAsTheGivenUser(t *testing.T) {
	stub, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	if err := c.Request(context.Background(), 3, Key{TV, 1399}, Choice{}); err != nil {
		t.Fatal(err)
	}
	if got := stub.last.Header.Get("X-API-User"); got != "3" {
		t.Errorf("X-API-User = %q, want 3", got)
	}
	if stub.last.URL.Path != "/api/v1/request" || stub.body["mediaType"] != "tv" || stub.body["mediaId"] != float64(1399) || stub.body["seasons"] != "all" {
		t.Errorf("TV request sent %s %v", stub.last.URL.Path, stub.body)
	}

	if err := c.Request(context.Background(), 1, Key{Movie, 603}, Choice{}); err != nil {
		t.Fatal(err)
	}
	if _, has := stub.body["seasons"]; has {
		t.Errorf("a movie request must not send seasons: %v", stub.body)
	}
	// No choice: no profile, folder or server, so Seerr decides.
	for _, f := range []string{"profileId", "rootFolder", "serverId"} {
		if _, has := stub.body[f]; has {
			t.Errorf("a plain request sent %s: %v", f, stub.body)
		}
	}
}

func TestRequestSendsTheChoice(t *testing.T) {
	stub, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	if err := c.Request(context.Background(), 1, Key{Movie, 603}, Choice{ProfileID: 13, RootFolder: "/media/movies-4k"}); err != nil {
		t.Fatal(err)
	}
	if stub.body["profileId"] != float64(13) || stub.body["rootFolder"] != "/media/movies-4k" {
		t.Fatalf("sent %v", stub.body)
	}
	// Without serverId Seerr applies the choice to its default server.
	if _, has := stub.body["serverId"]; has {
		t.Errorf("serverId sent: %v", stub.body)
	}
}

func TestWatchlistCallsActAsThePerson(t *testing.T) {
	stub, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"id":603,"title":"The Matrix","onUserWatchlist":true}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	ctx := context.Background()
	on, err := c.OnWatchlist(ctx, 3, Key{Movie, 603})
	if err != nil || !on || stub.last.Header.Get("X-API-User") != "3" {
		t.Fatalf("OnWatchlist = %v, %v as %q", on, err, stub.last.Header.Get("X-API-User"))
	}
	if err := c.SetWatchlist(ctx, 3, Key{Movie, 603}, "The Matrix", true); err != nil {
		t.Fatal(err)
	}
	if stub.last.Method != http.MethodPost || stub.last.URL.Path != "/api/v1/watchlist" || stub.body["tmdbId"] != float64(603) || stub.body["mediaType"] != "movie" || stub.last.Header.Get("X-API-User") != "3" {
		t.Errorf("add sent %s %s %v as %q", stub.last.Method, stub.last.URL.Path, stub.body, stub.last.Header.Get("X-API-User"))
	}
	if err := c.SetWatchlist(ctx, 3, Key{TV, 1399}, "", false); err != nil {
		t.Fatal(err)
	}
	if stub.last.Method != http.MethodDelete || stub.last.URL.Path != "/api/v1/watchlist/1399" || stub.last.URL.Query().Get("mediaType") != "tv" {
		t.Errorf("remove sent %s %s", stub.last.Method, stub.last.URL)
	}
	// Without a person it never reaches Seerr (it would act as the admin).
	before := stub.calls.Load()
	if err := c.SetWatchlist(ctx, 0, Key{Movie, 1}, "", true); err == nil || stub.calls.Load() != before {
		t.Error("watchlist change without a person reached Seerr")
	}
}

func TestRequestOptionsUseTheDefaultNon4KServer(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/service/sonarr":
			fmt.Fprint(w, `[{"id":0,"is4k":false,"isDefault":false},{"id":1,"is4k":true,"isDefault":true},{"id":2,"is4k":false,"isDefault":true}]`)
		case "/api/v1/service/sonarr/2":
			fmt.Fprint(w, `{"server":{"activeProfileId":9,"activeDirectory":"/media/tv"},
				"profiles":[{"id":8,"name":"HD-720p"},{"id":9,"name":"HD-1080p"}],
				"rootFolders":[{"id":1,"path":"/media/tv"},{"id":2,"path":"/media/tv-4k"}]}`)
		default:
			t.Errorf("asked %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	o, err := c.RequestOptions(context.Background(), TV)
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultProfile != 9 || o.DefaultFolder != "/media/tv" || len(o.Profiles) != 2 || strings.Join(o.RootFolders, ",") != "/media/tv,/media/tv-4k" {
		t.Fatalf("options %+v", o)
	}
	if !o.hasProfile(8) || o.hasProfile(7) || !o.hasFolder("/media/tv-4k") || o.hasFolder("/data") {
		t.Error("hasProfile/hasFolder disagree with the options")
	}
}

func TestRequestOptionsWithNoServer(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[{"id":1,"is4k":true,"isDefault":true}]`) })
	if _, err := c.RequestOptions(context.Background(), Movie); err == nil {
		t.Fatal("only a 4K server, yet options were returned")
	}
}

func TestAdvanced(t *testing.T) {
	for perms, want := range map[int]bool{
		0: false, 32: false, 288: false, // Bob's: request + auto-approve, not advanced
		permAdmin: true, permManageRequests: true, permRequestAdvanced: true, permRequestAdvanced | 32: true,
	} {
		if got := (SeerrUser{Permissions: perms}).advanced(); got != want {
			t.Errorf("permissions %d: advanced %v, want %v", perms, got, want)
		}
	}
}

func TestRequestRefusesWithoutAUser(t *testing.T) {
	stub, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	// Without X-API-User Seerr acts as its admin, so asking with no user must
	// never reach Seerr at all.
	if err := c.Request(context.Background(), 0, Key{Movie, 603}, Choice{}); err == nil {
		t.Fatal("request with no user accepted")
	}
	if n := stub.calls.Load(); n != 0 {
		t.Fatalf("Seerr was called %d time(s)", n)
	}
}

func TestSeerrErrorsCarrySeerrsMessage(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You do not have permission to request this."}`)
	})
	err := c.Request(context.Background(), 3, Key{Movie, 1}, Choice{})
	if !errors.Is(err, errSeerr) || userMessage(err) != "You do not have permission to request this." {
		t.Fatalf("error %v shown as %q", err, userMessage(err))
	}
}

func TestRecommendationsFallBackToSimilar(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/recommendations") {
			fmt.Fprint(w, `{"results":[]}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"id":7,"mediaType":"movie","title":"Similar one"},{"id":8,"mediaType":"person","name":"Not a title"}]}`)
	})
	got, err := c.Recommendations(context.Background(), Key{Movie, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Similar one" {
		t.Fatalf("got %+v, want only the similar movie (people dropped)", got)
	}
}

func TestRequestsPagesThroughAndSkipsBrokenEntries(t *testing.T) {
	_, c := newSeerrStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("requestedBy") != "3" {
			t.Errorf("requestedBy = %q", r.URL.Query().Get("requestedBy"))
		}
		var results []string
		n := seerrPageSize
		if r.URL.Query().Get("skip") != "0" {
			n = 2
		}
		for i := 0; i < n; i++ {
			results = append(results, fmt.Sprintf(`{"createdAt":"2026-11-01T10:00:00Z","media":{"tmdbId":%d,"mediaType":"movie"}}`, i+1))
		}
		results = append(results, `{"media":{"tmdbId":0,"mediaType":"movie"}}`, `{"media":{"tmdbId":5,"mediaType":"music"}}`)
		fmt.Fprintf(w, `{"pageInfo":{"pages":2,"page":%d},"results":[%s]}`, map[bool]int{true: 1, false: 2}[r.URL.Query().Get("skip") == "0"], strings.Join(results, ","))
	})
	got, err := c.Requests(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != seerrPageSize+2 {
		t.Fatalf("%d requests, want %d", len(got), seerrPageSize+2)
	}
}
