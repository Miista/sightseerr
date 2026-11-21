package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// jellyfinClient reads one thing: what a person has watched. It asks the same
// way reaparr does (reaparr/jellyfin.go, playedItems).
type jellyfinClient struct {
	base string
	key  string
	http *http.Client
}

func newJellyfinClient(base, key string) *jellyfinClient {
	return &jellyfinClient{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{Timeout: 60 * time.Second}}
}

type jellyfinItem struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	Type        string `json:"Type"` // "Movie", "Episode" or "Series"
	SeriesID    string `json:"SeriesId"`
	SeriesName  string `json:"SeriesName"`
	ProviderIds struct {
		Tmdb string `json:"Tmdb"`
	} `json:"ProviderIds"`
	UserData struct {
		LastPlayedDate *time.Time `json:"LastPlayedDate"`
	} `json:"UserData"`
}

// Watched is one title a person has played: a movie, or a show at least one
// episode of which was played.
type Watched struct {
	Key   Key
	Title string
	At    time.Time // last played; zero when Jellyfin didn't say
}

func (c *jellyfinClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	// The Authorization scheme, not the legacy X-Emby-Token header, which
	// Jellyfin 12.x rejects by default.
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, c.key))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin %s: %v", pathOnly(path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin %s answered %s", pathOnly(path), resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Watched is every title the Jellyfin user has played. Episodes are folded
// into their show, whose TMDb ID is looked up once per show (an episode only
// carries its own IDs). Items without a TMDb ID are skipped: there is nothing
// to recommend from.
func (c *jellyfinClient) Watched(ctx context.Context, userID string) ([]Watched, error) {
	var resp struct {
		Items []jellyfinItem `json:"Items"`
	}
	path := fmt.Sprintf("/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Filters=IsPlayed&Fields=SeriesId,ProviderIds",
		url.PathEscape(userID))
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}

	var out []Watched
	shows := map[string]*Watched{} // by Jellyfin series ID
	for _, it := range resp.Items {
		var at time.Time
		if it.UserData.LastPlayedDate != nil {
			at = *it.UserData.LastPlayedDate
		}
		switch it.Type {
		case "Movie":
			if id, err := strconv.Atoi(it.ProviderIds.Tmdb); err == nil && id > 0 {
				out = append(out, Watched{Key: Key{Movie, id}, Title: it.Name, At: at})
			}
		case "Episode":
			if it.SeriesID == "" {
				continue
			}
			if w, ok := shows[it.SeriesID]; ok {
				if at.After(w.At) {
					w.At = at
				}
				continue
			}
			shows[it.SeriesID] = &Watched{Key: Key{Type: TV}, Title: it.SeriesName, At: at}
		}
	}
	if len(shows) == 0 {
		return out, nil
	}

	ids := make([]string, 0, len(shows))
	for id := range shows {
		ids = append(ids, id)
	}
	// /Items filtered by Ids rather than /Items/{id}: the latter needs a user
	// context on newer Jellyfin. Asked in batches to keep the URL short.
	for start := 0; start < len(ids); start += 50 {
		batch := ids[start:min(start+50, len(ids))]
		var series struct {
			Items []jellyfinItem `json:"Items"`
		}
		if err := c.get(ctx, "/Items?Fields=ProviderIds&Ids="+url.QueryEscape(strings.Join(batch, ",")), &series); err != nil {
			return nil, err
		}
		for _, s := range series.Items {
			if w, ok := shows[s.ID]; ok {
				if id, err := strconv.Atoi(s.ProviderIds.Tmdb); err == nil && id > 0 {
					w.Key.ID = id
				}
			}
		}
	}
	for _, w := range shows {
		if w.Key.ID > 0 {
			out = append(out, *w)
		}
	}
	return out, nil
}
