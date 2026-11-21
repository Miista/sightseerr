package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// tmdbStub stands in for image.tmdb.org: it answers /w342/<name> with the
// name as the body, 404s "missing.jpg" and sends too much for "huge.jpg".
func tmdbStub(t *testing.T) (*imageStore, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/missing.jpg"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/huge.jpg"):
			w.Write(make([]byte, maxImageBytes+1))
		default:
			w.Write([]byte("image " + r.URL.Path))
		}
	}))
	t.Cleanup(srv.Close)
	s := newImageStore(filepath.Join(t.TempDir(), "images"))
	s.origin = srv.URL
	return s, &hits
}

func TestImagePathRefusesAnythingButTMDbImages(t *testing.T) {
	s := newImageStore("/data/images")
	if p, err := s.path("w342", "abc_DEF-1.jpg"); err != nil || p != "/data/images/w342/abc_DEF-1.jpg" {
		t.Fatalf("good image refused: %q, %v", p, err)
	}
	for _, c := range [][2]string{
		{"w999", "abc.jpg"},          // a size the page doesn't use
		{"original", "abc.jpg"},      // ditto
		{"w342", "../spor.db"},       // out of the folder
		{"w342", "..%2Fspor.db"},     // ditto, encoded
		{"w342", "a/b.jpg"},          // a path, not a name
		{"w342", "abc.svg"},          // not a TMDb image type
		{"w342", ".jpg"},             // no name
		{"w342", "x.jpg?u=http://e"}, // anything extra
	} {
		if _, err := s.path(c[0], c[1]); !errors.Is(err, errNoSuchImage) {
			t.Errorf("path(%q, %q) accepted", c[0], c[1])
		}
	}
}

func TestImagesAreFetchedOnceAndKept(t *testing.T) {
	s, hits := tmdbStub(t)
	for i := 0; i < 3; i++ {
		p, err := s.get(context.Background(), "w342", "poster.jpg")
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != "image /w342/poster.jpg" {
			t.Fatalf("stored %q", b)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("TMDb asked %d times, want once", n)
	}
}

func TestConcurrentAsksShareOneDownload(t *testing.T) {
	s, hits := tmdbStub(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.get(context.Background(), "w342", "same.jpg"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := hits.Load(); n != 1 {
		t.Fatalf("TMDb asked %d times for one image, want once", n)
	}
}

func TestImageFailuresStoreNothing(t *testing.T) {
	s, _ := tmdbStub(t)
	if _, err := s.get(context.Background(), "w342", "missing.jpg"); !errors.Is(err, errNoSuchImage) {
		t.Errorf("missing image: %v, want errNoSuchImage", err)
	}
	if _, err := s.get(context.Background(), "w342", "huge.jpg"); err == nil {
		t.Error("oversized image accepted")
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "w342"))
	for _, e := range entries {
		t.Errorf("left behind %s", e.Name())
	}
}

func TestImageRouteServesAndCaches(t *testing.T) {
	s, _ := tmdbStub(t)
	h := newHarness(t)
	h.srv.images = s
	as(t, "alice@example.com")
	w, _ := h.do(t, "GET", "/img/w342/poster.jpg", "")
	if w.Code != http.StatusOK || w.Body.String() != "image /w342/poster.jpg" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("got %d %q (Cache-Control %q)", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/img/w999/poster.jpg", "/img/w342/missing.jpg", "/img/w342/..%2Fspor.db"} {
		if w, _ := h.do(t, "GET", p, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}
