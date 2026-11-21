package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// statusBook is Seerr's status for every title it has a record of, refreshed
// by the sync and nudged when spor itself makes a request.
type statusBook struct {
	mu       sync.RWMutex
	statuses map[Key]int
	library  []Key // available or partly available, newest first
}

func (b *statusBook) set(items []LibraryItem) {
	m := make(map[Key]int, len(items))
	var lib []Key
	for _, it := range items {
		m[it.Key] = it.Status
		if it.Status == statusAvailable || it.Status == statusPartial {
			lib = append(lib, it.Key)
		}
	}
	b.mu.Lock()
	b.statuses, b.library = m, lib
	b.mu.Unlock()
}

func (b *statusBook) snapshot() map[Key]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m := make(map[Key]int, len(b.statuses))
	for k, v := range b.statuses {
		m[k] = v
	}
	return m
}

func (b *statusBook) get(k Key) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.statuses[k]
}

func (b *statusBook) mark(k Key, status int) {
	b.mu.Lock()
	if b.statuses == nil {
		b.statuses = map[Key]int{}
	}
	if b.statuses[k] < status {
		b.statuses[k] = status
	}
	b.mu.Unlock()
}

func (b *statusBook) libraryKeys() []Key {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Key(nil), b.library...)
}

// syncer copies what Jellyfin and Seerr know about each person into the store,
// so a reap (which makes both forget) loses nothing.
type syncer struct {
	seerr    *cachedSeerr
	jellyfin *jellyfinClient
	store    *Store
	people   *directory
	statuses *statusBook
	log      zerolog.Logger

	mu    sync.Mutex
	ready bool // at least one complete sync
}

func (s *syncer) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *syncer) run(ctx context.Context, every time.Duration) {
	for {
		start := time.Now()
		if err := s.once(ctx); err != nil {
			s.log.Warn().Msg(fmt.Sprintf("sync did not finish, trying again in %s: %v", every, err))
		} else {
			s.log.Debug().Msg(fmt.Sprintf("sync finished in %s", time.Since(start).Round(time.Millisecond)))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// once is one complete sync. Seerr's user list and statuses come first, since
// everything else hangs off them; a failure for one person is logged and the
// others still sync.
func (s *syncer) once(ctx context.Context) error {
	users, err := s.seerr.Users(ctx)
	if err != nil {
		return err
	}
	s.people.set(users)
	media, err := s.seerr.libraryAll(ctx)
	if err != nil {
		return err
	}
	s.statuses.set(media)

	for _, u := range users {
		if err := s.person(ctx, u); err != nil {
			s.log.Warn().Msg(fmt.Sprintf("could not sync %s: %v", nameOf(u), err))
		}
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	go s.warm(context.WithoutCancel(ctx), users)
	return nil
}

func nameOf(u SeerrUser) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return fmt.Sprintf("Seerr user %d", u.ID)
}

func (s *syncer) person(ctx context.Context, u SeerrUser) error {
	if u.JellyfinUserID != "" {
		watched, err := s.jellyfin.Watched(ctx, u.JellyfinUserID)
		if err != nil {
			return err
		}
		if err := s.store.AddWatched(u.ID, watched); err != nil {
			return err
		}
	}
	reqs, err := s.seerr.Requests(ctx, u.ID)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if err := s.store.AddRequested(u.ID, r.Key, "", r.At); err != nil {
			return err
		}
	}
	return s.fillTitles(ctx, u.ID)
}

// fillTitles names seeds that came without a name (requests carry only IDs),
// so a card can say "because you requested Dune".
func (s *syncer) fillTitles(ctx context.Context, user int) error {
	seeds, err := s.store.Seeds(user)
	if err != nil {
		return err
	}
	for _, sd := range seeds {
		if sd.Title != "" {
			continue
		}
		d, err := s.seerr.Details(ctx, sd.Key)
		if err != nil {
			s.log.Debug().Msg(fmt.Sprintf("could not name %s yet: %v", sd.Key, err))
			continue
		}
		if err := s.store.SetTitle(user, sd.Key, d.Title.Title); err != nil {
			return err
		}
	}
	return nil
}

// warm fetches recommendations for everyone's seeds ahead of time, so the
// first wall of the day doesn't wait on dozens of Seerr calls. Errors are
// ignored: the wall fetches whatever is missing itself.
func (s *syncer) warm(ctx context.Context, users []SeerrUser) {
	for _, u := range users {
		seeds, err := s.store.Seeds(u.ID)
		if err != nil {
			continue
		}
		for _, sd := range seeds {
			if ctx.Err() != nil {
				return
			}
			s.seerr.Recommendations(ctx, sd.Key)
		}
	}
}
