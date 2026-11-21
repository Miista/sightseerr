package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MediaType is TMDb's split: a movie or a TV show. A TMDb ID only means
// something together with its type, since the two are numbered separately.
type MediaType string

const (
	Movie MediaType = "movie"
	TV    MediaType = "tv"
)

func parseMediaType(s string) (MediaType, bool) {
	switch MediaType(s) {
	case Movie, TV:
		return MediaType(s), true
	}
	return "", false
}

// Key names one title: its type and TMDb ID.
type Key struct {
	Type MediaType
	ID   int
}

func (k Key) String() string { return fmt.Sprintf("%s:%d", k.Type, k.ID) }

// parseKey reads "movie:603" or "tv:1399".
func parseKey(s string) (Key, bool) {
	t, id, ok := strings.Cut(s, ":")
	if !ok {
		return Key{}, false
	}
	mt, ok := parseMediaType(t)
	if !ok {
		return Key{}, false
	}
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return Key{}, false
	}
	return Key{mt, n}, true
}

// Seerr's media statuses (server/constants/media.ts). A title Seerr has no
// record of has no status at all, which reads as 0 here.
const (
	statusUnknown     = 1
	statusPending     = 2
	statusProcessing  = 3
	statusPartial     = 4
	statusAvailable   = 5
	statusBlocklisted = 6
	statusDeleted     = 7
)

// inLibrary reports whether Seerr's status means the title is had, on its
// way, or deliberately kept out: anything spor must not offer as new.
// Unknown and deleted (and no record) are fair game.
func inLibrary(status int) bool {
	switch status {
	case statusPending, statusProcessing, statusPartial, statusAvailable, statusBlocklisted:
		return true
	}
	return false
}

// Title is one movie or show as the page shows it on a card.
type Title struct {
	Type     MediaType `json:"type"`
	ID       int       `json:"id"`
	Title    string    `json:"title"`
	Year     string    `json:"year,omitempty"`
	Poster   string    `json:"poster,omitempty"` // TMDb image path, e.g. "/abc.jpg"
	Overview string    `json:"overview,omitempty"`
	Status   int       `json:"status"`
}

func (t Title) Key() Key { return Key{t.Type, t.ID} }

// Details is a title's page: the card plus what only the page shows.
type Details struct {
	Title
	Backdrop string   `json:"backdrop,omitempty"`
	Tagline  string   `json:"tagline,omitempty"`
	Runtime  int      `json:"runtime,omitempty"` // minutes, movies only
	Seasons  int      `json:"seasons,omitempty"` // TV only
	Genres   []string `json:"genres,omitempty"`
	Rating   float64  `json:"rating,omitempty"` // TMDb vote average
	IMDb     string   `json:"-"`
	// Collection is the TMDb collection a movie belongs to ("Star Trek:
	// Alternate Reality Collection"); nil for shows and standalone movies.
	Collection *Collection `json:"collection,omitempty"`
}

type Collection struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// seerrTitle is the subset of Seerr's search/recommendation results and
// detail pages spor reads. Movies say title/releaseDate, shows name/firstAirDate.
type seerrTitle struct {
	ID              int     `json:"id"`
	MediaType       string  `json:"mediaType"`
	Title           string  `json:"title"`
	Name            string  `json:"name"`
	ReleaseDate     string  `json:"releaseDate"`
	FirstAirDate    string  `json:"firstAirDate"`
	PosterPath      string  `json:"posterPath"`
	BackdropPath    string  `json:"backdropPath"`
	Overview        string  `json:"overview"`
	Tagline         string  `json:"tagline"`
	Runtime         int     `json:"runtime"`
	NumberOfSeasons int     `json:"numberOfSeasons"`
	VoteAverage     float64 `json:"voteAverage"`
	ImdbID          string  `json:"imdbId"`
	ExternalIDs     struct {
		ImdbID string `json:"imdbId"`
	} `json:"externalIds"`
	Genres []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Collection *Collection `json:"collection"`
	MediaInfo  *struct {
		Status int `json:"status"`
	} `json:"mediaInfo"`
}

func (s seerrTitle) title(fallback MediaType) Title {
	t := Title{Type: fallback, ID: s.ID, Title: s.Title, Poster: s.PosterPath, Overview: s.Overview}
	if mt, ok := parseMediaType(s.MediaType); ok {
		t.Type = mt
	}
	date := s.ReleaseDate
	if t.Type == TV {
		t.Title, date = s.Name, s.FirstAirDate
	}
	if len(date) >= 4 {
		t.Year = date[:4]
	}
	if s.MediaInfo != nil {
		t.Status = s.MediaInfo.Status
	}
	return t
}

func (s seerrTitle) details(mt MediaType) Details {
	d := Details{
		Title: s.title(mt), Backdrop: s.BackdropPath, Tagline: s.Tagline,
		Rating: s.VoteAverage, IMDb: s.ImdbID,
	}
	if mt == Movie {
		d.Runtime = s.Runtime
		if s.Collection != nil && s.Collection.ID > 0 {
			d.Collection = s.Collection
		}
	} else {
		d.Seasons = s.NumberOfSeasons
	}
	if d.IMDb == "" {
		d.IMDb = s.ExternalIDs.ImdbID
	}
	for _, g := range s.Genres {
		d.Genres = append(d.Genres, g.Name)
	}
	return d
}

// SeerrUser is a Seerr account and the Jellyfin account it is linked to.
type SeerrUser struct {
	ID             int    `json:"id"`
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	JellyfinUserID string `json:"jellyfinUserId"`
	Permissions    int    `json:"permissions"`
}

// Seerr permission bits (server/lib/permissions.ts) that let a person choose
// the quality profile and root folder of a request. Seerr ignores the choice
// from anyone else, so spor only offers it to these.
const (
	permAdmin           = 2
	permManageRequests  = 16
	permRequestAdvanced = 8192
)

func (u SeerrUser) advanced() bool {
	return u.Permissions&(permAdmin|permManageRequests|permRequestAdvanced) != 0
}

// Profile is a quality profile on the Radarr/Sonarr server.
type Profile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RequestOptions is what an advanced request may choose for one media type:
// the default (non-4K) server's quality profiles and root folders, and the
// server's defaults.
type RequestOptions struct {
	Profiles       []Profile `json:"profiles"`
	RootFolders    []string  `json:"rootFolders"`
	DefaultProfile int       `json:"defaultProfile"`
	DefaultFolder  string    `json:"defaultFolder"`
}

func (o RequestOptions) hasProfile(id int) bool {
	for _, p := range o.Profiles {
		if p.ID == id {
			return true
		}
	}
	return false
}

func (o RequestOptions) hasFolder(path string) bool {
	for _, f := range o.RootFolders {
		if f == path {
			return true
		}
	}
	return false
}

// Choice is what an advanced request asks for; zero values mean "let Seerr
// decide", and are not sent.
type Choice struct {
	ProfileID  int
	RootFolder string
}

// LibraryItem is one entry of Seerr's media list: no title or poster, only
// what Seerr tracks.
type LibraryItem struct {
	Key    Key
	Status int
}

// SeerrRequest is one request a person made.
type SeerrRequest struct {
	Key Key
	At  time.Time
}

// errSeerr wraps every failure talking to Seerr; its message is Seerr's own
// when Seerr sent one (e.g. "You do not have permission to make 4K requests").
var errSeerr = errors.New("seerr")

// seerrClient talks to Seerr's API with its API key. The key is admin-level,
// so asUser (X-API-User) is how spor acts for a person; it must only ever
// come from the signed-in session's mapping.
type seerrClient struct {
	base string
	key  string
	http *http.Client
}

func newSeerrClient(base, key string) *seerrClient {
	return &seerrClient{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *seerrClient) do(ctx context.Context, method, path string, asUser int, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if asUser > 0 {
		req.Header.Set("X-API-User", strconv.Itoa(asUser))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", errSeerr, method, pathOnly(path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&e)
		if e.Message != "" {
			return fmt.Errorf("%w: %s", errSeerr, e.Message)
		}
		return fmt.Errorf("%w: %s %s answered %s", errSeerr, method, pathOnly(path), resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: %s %s: unreadable answer: %v", errSeerr, method, pathOnly(path), err)
	}
	return nil
}

// pathOnly drops the query, which is never logged.
func pathOnly(p string) string {
	path, _, _ := strings.Cut(p, "?")
	return path
}

func (c *seerrClient) Users(ctx context.Context) ([]SeerrUser, error) {
	var out struct {
		Results []SeerrUser `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, "/user?take=1000", 0, nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

const seerrPageSize = 100

// Requests is every request the Seerr user has made that Seerr still has.
func (c *seerrClient) Requests(ctx context.Context, userID int) ([]SeerrRequest, error) {
	var all []SeerrRequest
	for skip := 0; ; skip += seerrPageSize {
		var out struct {
			PageInfo struct {
				Pages int `json:"pages"`
				Page  int `json:"page"`
			} `json:"pageInfo"`
			Results []struct {
				CreatedAt time.Time `json:"createdAt"`
				Media     struct {
					TmdbID    int    `json:"tmdbId"`
					MediaType string `json:"mediaType"`
				} `json:"media"`
			} `json:"results"`
		}
		path := fmt.Sprintf("/request?take=%d&skip=%d&requestedBy=%d&sort=added", seerrPageSize, skip, userID)
		if err := c.do(ctx, http.MethodGet, path, 0, nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Results {
			mt, ok := parseMediaType(r.Media.MediaType)
			if !ok || r.Media.TmdbID <= 0 {
				continue
			}
			all = append(all, SeerrRequest{Key: Key{mt, r.Media.TmdbID}, At: r.CreatedAt})
		}
		if len(out.Results) < seerrPageSize || out.PageInfo.Page >= out.PageInfo.Pages {
			return all, nil
		}
	}
}

// libraryAll is every title Seerr has a record of, with its status, newest
// first: what's had, on its way, blocklisted or forgotten.
func (c *seerrClient) libraryAll(ctx context.Context) ([]LibraryItem, error) {
	var all []LibraryItem
	for skip := 0; ; skip += seerrPageSize {
		var out struct {
			PageInfo struct {
				Pages int `json:"pages"`
				Page  int `json:"page"`
			} `json:"pageInfo"`
			Results []struct {
				TmdbID    int    `json:"tmdbId"`
				MediaType string `json:"mediaType"`
				Status    int    `json:"status"`
			} `json:"results"`
		}
		path := fmt.Sprintf("/media?take=%d&skip=%d&filter=all&sort=added", seerrPageSize, skip)
		if err := c.do(ctx, http.MethodGet, path, 0, nil, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Results {
			mt, ok := parseMediaType(m.MediaType)
			if !ok || m.TmdbID <= 0 {
				continue
			}
			all = append(all, LibraryItem{Key: Key{mt, m.TmdbID}, Status: m.Status})
		}
		if len(out.Results) < seerrPageSize || out.PageInfo.Page >= out.PageInfo.Pages {
			return all, nil
		}
	}
}

func (c *seerrClient) Details(ctx context.Context, k Key) (Details, error) {
	var s seerrTitle
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/%s/%d", k.Type, k.ID), 0, nil, &s); err != nil {
		return Details{}, err
	}
	return s.details(k.Type), nil
}

// Recommendations is TMDb's "recommended" list for the title (first page),
// falling back to "similar" when TMDb has no recommendations for it.
func (c *seerrClient) Recommendations(ctx context.Context, k Key) ([]Title, error) {
	for _, kind := range []string{"recommendations", "similar"} {
		var out struct {
			Results []seerrTitle `json:"results"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/%s/%d/%s?page=1", k.Type, k.ID, kind), 0, nil, &out); err != nil {
			return nil, err
		}
		if len(out.Results) == 0 {
			continue
		}
		titles := make([]Title, 0, len(out.Results))
		for _, r := range out.Results {
			// A result that says what it is must be a movie or show (TMDb lists
			// can carry people); one that doesn't is the asked-for type.
			if _, ok := parseMediaType(r.MediaType); r.MediaType != "" && !ok {
				continue
			}
			if t := r.title(k.Type); t.ID > 0 {
				titles = append(titles, t)
			}
		}
		return titles, nil
	}
	return nil, nil
}

// Search is Seerr's search (first page). Besides words it takes "tvdb:123",
// "imdb:tt123" and "tmdb:123", which the backfill uses to find a show by ID.
func (c *seerrClient) Search(ctx context.Context, query string) ([]Title, error) {
	var out struct {
		Results []seerrTitle `json:"results"`
	}
	if err := c.do(ctx, http.MethodGet, "/search?page=1&query="+url.QueryEscape(query), 0, nil, &out); err != nil {
		return nil, err
	}
	var titles []Title
	for _, r := range out.Results {
		if _, ok := parseMediaType(r.MediaType); ok && r.ID > 0 {
			titles = append(titles, r.title(Movie))
		}
	}
	return titles, nil
}

// CollectionParts is every movie in a TMDb collection, oldest first, with
// the status Seerr has for each.
func (c *seerrClient) CollectionParts(ctx context.Context, id int) ([]Title, error) {
	var out struct {
		Parts []seerrTitle `json:"parts"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/collection/%d", id), 0, nil, &out); err != nil {
		return nil, err
	}
	type dated struct {
		t    Title
		date string
	}
	parts := make([]dated, 0, len(out.Parts))
	for _, p := range out.Parts {
		if t := p.title(Movie); t.ID > 0 {
			parts = append(parts, dated{t, p.ReleaseDate})
		}
	}
	// Unreleased parts have no date yet; they go last, as they come out last.
	slices.SortStableFunc(parts, func(a, b dated) int {
		switch {
		case a.date == b.date:
			return 0
		case a.date == "":
			return 1
		case b.date == "":
			return -1
		}
		return strings.Compare(a.date, b.date)
	})
	titles := make([]Title, len(parts))
	for i, p := range parts {
		titles[i] = p.t
	}
	return titles, nil
}

// Request asks Seerr for the title as the given Seerr user, so Seerr applies
// that person's permissions and auto-approval. A show is requested whole.
// The choice's fields are sent only when set; with no serverId sent, Seerr
// applies them to its default server (server/entity/MediaRequest.ts).
func (c *seerrClient) Request(ctx context.Context, asUser int, k Key, ch Choice) error {
	if asUser <= 0 {
		return fmt.Errorf("%w: no Seerr user to request as", errSeerr)
	}
	body := map[string]any{"mediaType": k.Type, "mediaId": k.ID}
	if k.Type == TV {
		body["seasons"] = "all"
	}
	if ch.ProfileID > 0 {
		body["profileId"] = ch.ProfileID
	}
	if ch.RootFolder != "" {
		body["rootFolder"] = ch.RootFolder
	}
	return c.do(ctx, http.MethodPost, "/request", asUser, body, nil)
}

// service is the Arr app Seerr uses for the media type.
func service(mt MediaType) string {
	if mt == TV {
		return "sonarr"
	}
	return "radarr"
}

// RequestOptions reads the default non-4K server's profiles and root folders.
func (c *seerrClient) RequestOptions(ctx context.Context, mt MediaType) (RequestOptions, error) {
	var servers []struct {
		ID        int  `json:"id"`
		Is4K      bool `json:"is4k"`
		IsDefault bool `json:"isDefault"`
	}
	if err := c.do(ctx, http.MethodGet, "/service/"+service(mt), 0, nil, &servers); err != nil {
		return RequestOptions{}, err
	}
	id := -1
	for _, s := range servers {
		if s.Is4K {
			continue
		}
		if id < 0 || s.IsDefault {
			id = s.ID
		}
		if s.IsDefault {
			break
		}
	}
	if id < 0 {
		return RequestOptions{}, fmt.Errorf("%w: no %s server is set up in Seerr", errSeerr, service(mt))
	}
	var out struct {
		Server struct {
			ActiveProfileID int    `json:"activeProfileId"`
			ActiveDirectory string `json:"activeDirectory"`
		} `json:"server"`
		Profiles    []Profile `json:"profiles"`
		RootFolders []struct {
			Path string `json:"path"`
		} `json:"rootFolders"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/service/%s/%d", service(mt), id), 0, nil, &out); err != nil {
		return RequestOptions{}, err
	}
	o := RequestOptions{Profiles: out.Profiles, DefaultProfile: out.Server.ActiveProfileID, DefaultFolder: out.Server.ActiveDirectory}
	if o.Profiles == nil {
		o.Profiles = []Profile{}
	}
	o.RootFolders = []string{}
	for _, f := range out.RootFolders {
		o.RootFolders = append(o.RootFolders, f.Path)
	}
	return o, nil
}
