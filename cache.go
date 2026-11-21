package main

import (
	"context"
	"sync"
	"time"
)

// ttlCache remembers answers per title for a while, and makes concurrent asks
// for the same title wait for one fetch. Failures are not remembered.
type ttlCache[V any] struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	items    map[Key]cached[V]
	inflight map[Key]*fetch[V]
	puts     int
}

type cached[V any] struct {
	v   V
	exp time.Time
}

type fetch[V any] struct {
	done chan struct{}
	v    V
	err  error
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, now: time.Now, items: map[Key]cached[V]{}, inflight: map[Key]*fetch[V]{}}
}

func (c *ttlCache[V]) get(ctx context.Context, k Key, load func(context.Context, Key) (V, error)) (V, error) {
	c.mu.Lock()
	if it, ok := c.items[k]; ok && c.now().Before(it.exp) {
		c.mu.Unlock()
		return it.v, nil
	}
	if f, ok := c.inflight[k]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.v, f.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	f := &fetch[V]{done: make(chan struct{})}
	c.inflight[k] = f
	c.mu.Unlock()

	// Detached from the asker's context: others may be waiting on this fetch,
	// and one closed browser tab shouldn't fail it for them.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	f.v, f.err = load(fctx, k)
	cancel()

	c.mu.Lock()
	delete(c.inflight, k)
	if f.err == nil {
		c.items[k] = cached[V]{v: f.v, exp: c.now().Add(c.ttl)}
		if c.puts++; c.puts%500 == 0 {
			c.prune()
		}
	}
	c.mu.Unlock()
	close(f.done)
	return f.v, f.err
}

// prune drops expired entries; called with mu held.
func (c *ttlCache[V]) prune() {
	now := c.now()
	for k, it := range c.items {
		if !now.Before(it.exp) {
			delete(c.items, k)
		}
	}
}

// cachedSeerr puts caches in front of the two Seerr calls a page view makes
// most. Statuses inside cached answers can be stale, so callers always check
// titles against the status map too (see Exclusions).
type cachedSeerr struct {
	*seerrClient
	recs    *ttlCache[[]Title]
	details *ttlCache[Details]
	options *ttlCache[RequestOptions] // keyed by media type, ID 0
	parts   *ttlCache[[]Title]        // collection parts, keyed by {movie, collection ID}
	ratings *ttlCache[Ratings]
}

func (c *cachedSeerr) Ratings(ctx context.Context, k Key) (Ratings, error) {
	return c.ratings.get(ctx, k, c.seerrClient.Ratings)
}

func (c *cachedSeerr) CollectionParts(ctx context.Context, id int) ([]Title, error) {
	return c.parts.get(ctx, Key{Movie, id}, func(ctx context.Context, k Key) ([]Title, error) {
		return c.seerrClient.CollectionParts(ctx, k.ID)
	})
}

func newCachedSeerr(c *seerrClient) *cachedSeerr {
	return &cachedSeerr{
		seerrClient: c, recs: newTTLCache[[]Title](24 * time.Hour), details: newTTLCache[Details](24 * time.Hour),
		// Short: a profile added in Radarr should show up soon.
		options: newTTLCache[RequestOptions](10 * time.Minute),
		parts:   newTTLCache[[]Title](24 * time.Hour),
		ratings: newTTLCache[Ratings](24 * time.Hour),
	}
}

func (c *cachedSeerr) RequestOptions(ctx context.Context, mt MediaType) (RequestOptions, error) {
	return c.options.get(ctx, Key{Type: mt}, func(ctx context.Context, k Key) (RequestOptions, error) {
		return c.seerrClient.RequestOptions(ctx, k.Type)
	})
}

func (c *cachedSeerr) Recommendations(ctx context.Context, k Key) ([]Title, error) {
	return c.recs.get(ctx, k, c.seerrClient.Recommendations)
}

func (c *cachedSeerr) Details(ctx context.Context, k Key) (Details, error) {
	return c.details.get(ctx, k, c.seerrClient.Details)
}
