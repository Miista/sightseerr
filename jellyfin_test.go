package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWatchedFoldsEpisodesIntoShows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="jk"` {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/Users/u1/Items"):
			if r.URL.Query().Get("Filters") != "IsPlayed" {
				t.Errorf("asked without the IsPlayed filter: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"Items":[
				{"Id":"m1","Name":"Heat","Type":"Movie","ProviderIds":{"Tmdb":"949"},"UserData":{"LastPlayedDate":"2026-11-02T20:00:00Z"}},
				{"Id":"m2","Name":"Home video","Type":"Movie","ProviderIds":{}},
				{"Id":"e1","Type":"Episode","SeriesId":"s1","SeriesName":"Dark","UserData":{"LastPlayedDate":"2026-11-01T20:00:00Z"}},
				{"Id":"e2","Type":"Episode","SeriesId":"s1","SeriesName":"Dark","UserData":{"LastPlayedDate":"2026-11-05T20:00:00Z"}},
				{"Id":"e3","Type":"Episode","SeriesId":"s2","SeriesName":"No TMDb"}
			]}`)
		case r.URL.Path == "/Items":
			fmt.Fprint(w, `{"Items":[{"Id":"s1","ProviderIds":{"Tmdb":"70523"}},{"Id":"s2","ProviderIds":{}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, err := newJellyfinClient(srv.URL, "jk").Watched(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[Key]Watched{}
	for _, w := range got {
		byKey[w.Key] = w
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want Heat and Dark only (no TMDb ID = skipped)", got)
	}
	if w, ok := byKey[Key{Movie, 949}]; !ok || w.Title != "Heat" {
		t.Errorf("Heat missing or wrong: %+v", w)
	}
	dark, ok := byKey[Key{TV, 70523}]
	if !ok || dark.Title != "Dark" || !dark.At.Equal(time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)) {
		t.Errorf("Dark = %+v, want the show dated by its latest episode", dark)
	}
}

func TestWatchedFailsOnJellyfinError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	if got, err := newJellyfinClient(srv.URL, "wrong").Watched(context.Background(), "u1"); err == nil || got != nil {
		t.Fatalf("got %v, %v; want an error and nothing", got, err)
	}
}
