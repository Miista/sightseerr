package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// imageStore serves TMDb posters and backdrops from spor itself, so the page
// never talks to image.tmdb.org: each image is fetched once, kept on disk and
// served from there. A TMDb image path names one fixed picture (a new picture
// gets a new path), so a stored copy never goes stale.
type imageStore struct {
	dir    string // e.g. /app/data/images
	origin string // https://image.tmdb.org/t/p
	http   *http.Client

	mu       sync.Mutex
	inflight map[string]*sync.WaitGroup
}

// The sizes the page asks for, and what a TMDb file name looks like. Nothing
// else is fetched: the endpoint can't be pointed at another URL or file.
var (
	imageSizes = map[string]bool{"w154": true, "w342": true, "w1280": true}
	imageName  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\.(jpg|png|webp)$`)
)

const maxImageBytes = 10 << 20

func newImageStore(dir string) *imageStore {
	return &imageStore{dir: dir, origin: "https://image.tmdb.org/t/p", http: &http.Client{Timeout: 30 * time.Second}, inflight: map[string]*sync.WaitGroup{}}
}

var errNoSuchImage = errors.New("no such image")

// path is where an image lives on disk, or an error for anything that isn't a
// TMDb image in an allowed size.
func (s *imageStore) path(size, name string) (string, error) {
	if !imageSizes[size] || !imageName.MatchString(name) {
		return "", errNoSuchImage
	}
	return filepath.Join(s.dir, size, name), nil
}

// get makes sure the image is on disk and returns its path. Concurrent asks
// for the same image wait for one download.
func (s *imageStore) get(ctx context.Context, size, name string) (string, error) {
	p, err := s.path(size, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	s.mu.Lock()
	if wg, ok := s.inflight[p]; ok {
		s.mu.Unlock()
		wg.Wait()
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("image %s/%s could not be fetched", size, name)
		}
		return p, nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	s.inflight[p] = wg
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, p)
		s.mu.Unlock()
		wg.Done()
	}()
	if err := s.fetch(context.WithoutCancel(ctx), size, name, p); err != nil {
		return "", err
	}
	return p, nil
}

// fetch downloads to a temporary file and renames it into place, so a
// half-written image is never served.
func (s *imageStore) fetch(ctx context.Context, size, name, p string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+"/"+size+"/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetching image %s/%s: %v", size, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoSuchImage
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching image %s/%s: TMDb answered %s", size, name, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxImageBytes {
		return fmt.Errorf("image %s/%s is larger than %d bytes", size, name, maxImageBytes)
	}
	return os.Rename(tmp.Name(), p)
}

// serve is GET /img/{size}/{name}.
func (s *imageStore) serve(w http.ResponseWriter, r *http.Request) {
	p, err := s.get(r.Context(), r.PathValue("size"), r.PathValue("name"))
	switch {
	case errors.Is(err, errNoSuchImage):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	// The same path is always the same picture.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, p)
}
