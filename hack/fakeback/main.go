// fakeback stands in for Seerr (:5055) and Jellyfin (:8096) so spor can be
// run and looked at locally without touching the real ones. titles.json is
// public TMDb metadata (as Seerr returns it); everything about people,
// watches, requests and the library is made up here.
//
//	go run ./hack/fakeback
//	SPOR_SEERR_URL=http://localhost:5055 SPOR_SEERR_API_KEY=x \
//	SPOR_JELLYFIN_URL=http://localhost:8096 SPOR_JELLYFIN_API_KEY=x \
//	SPOR_AUTH_DISABLED=true SPOR_ANONYMOUS_USER=1 SPOR_DATA_DIR=/tmp/spor \
//	SPOR_ADDR=:8080 go run .
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed titles.json
var titlesJSON []byte

type title map[string]any

func (t title) typ() string {
	if t["mediaType"] == "tv" || t["name"] != nil && t["title"] == nil {
		return "tv"
	}
	return "movie"
}

func (t title) id() int { return int(t["id"].(float64)) }

var (
	byKey   = map[string]title{}
	ofType  = map[string][]title{}
	mu      sync.Mutex
	status  = map[string]int{"movie:27205": 5, "movie:604": 5, "movie:218": 5, "tv:1399": 3} // Inception (watched), Reloaded and The Terminator (not) are had; GoT requested
	reqs    = []string{"tv:1399"}
	watched = []map[string]any{
		{"Id": "m1", "Name": "The Matrix", "Type": "Movie", "ProviderIds": map[string]string{"Tmdb": "603"}, "UserData": map[string]any{"LastPlayedDate": "2026-11-10T20:00:00Z"}},
		{"Id": "m2", "Name": "Interstellar", "Type": "Movie", "ProviderIds": map[string]string{"Tmdb": "157336"}, "UserData": map[string]any{"LastPlayedDate": "2026-11-15T20:00:00Z"}},
		{"Id": "m3", "Name": "Inception", "Type": "Movie", "ProviderIds": map[string]string{"Tmdb": "27205"}, "UserData": map[string]any{"LastPlayedDate": "2026-11-01T20:00:00Z"}},
		{"Id": "e1", "Type": "Episode", "SeriesId": "s1", "SeriesName": "Dark", "UserData": map[string]any{"LastPlayedDate": "2026-11-18T20:00:00Z"}},
	}
)

func main() {
	var all []title
	if err := json.Unmarshal(titlesJSON, &all); err != nil {
		log.Fatal(err)
	}
	for _, t := range all {
		k := fmt.Sprintf("%s:%d", t.typ(), t.id())
		if _, dup := byKey[k]; dup {
			continue
		}
		t["mediaType"] = t.typ()
		byKey[k] = t
		ofType[t.typ()] = append(ofType[t.typ()], t)
	}
	for _, list := range ofType {
		sort.Slice(list, func(i, j int) bool { return list[i].id() < list[j].id() })
	}
	go func() { log.Fatal(http.ListenAndServe(":8096", http.HandlerFunc(jellyfin))) }()
	log.Printf("fake Seerr on :5055, fake Jellyfin on :8096, %d titles", len(byKey))
	log.Fatal(http.ListenAndServe(":5055", http.HandlerFunc(seerr)))
}

func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func withStatus(t title) title {
	out := title{}
	for k, v := range t {
		out[k] = v
	}
	mu.Lock()
	if s, ok := status[fmt.Sprintf("%s:%d", t.typ(), t.id())]; ok {
		out["mediaInfo"] = map[string]int{"status": s}
	}
	mu.Unlock()
	return out
}

func seerr(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.Split(p, "/")
	switch {
	case p == "user":
		send(w, map[string]any{"results": []map[string]any{{"id": 1, "email": "alice@example.com", "displayName": "Alice", "jellyfinUserId": "u1", "permissions": 2}}})
	case p == "service/radarr" || p == "service/sonarr":
		send(w, []map[string]any{{"id": 0, "name": parts[1], "is4k": false, "isDefault": true}})
	case p == "service/radarr/0":
		send(w, map[string]any{"server": map[string]any{"activeProfileId": 8, "activeDirectory": "/media/movies"},
			"profiles":    []map[string]any{{"id": 8, "name": "HD-1080p"}, {"id": 12, "name": "Ultra-HD"}, {"id": 13, "name": "Remux"}},
			"rootFolders": []map[string]any{{"id": 1, "path": "/media/movies"}, {"id": 2, "path": "/media/movies-4k"}}})
	case p == "service/sonarr/0":
		send(w, map[string]any{"server": map[string]any{"activeProfileId": 9, "activeDirectory": "/media/tv"},
			"profiles":    []map[string]any{{"id": 8, "name": "HD-720p"}, {"id": 9, "name": "HD-1080p"}, {"id": 11, "name": "Remux"}},
			"rootFolders": []map[string]any{{"id": 1, "path": "/media/tv"}, {"id": 2, "path": "/media/tv-4k"}}})
	case p == "media":
		mu.Lock()
		var res []map[string]any
		for k, s := range status {
			typ, id, _ := strings.Cut(k, ":")
			n, _ := strconv.Atoi(id)
			res = append(res, map[string]any{"tmdbId": n, "mediaType": typ, "status": s})
		}
		mu.Unlock()
		send(w, map[string]any{"pageInfo": map[string]int{"page": 1, "pages": 1}, "results": res})
	case p == "request" && r.Method == http.MethodGet:
		mu.Lock()
		var res []map[string]any
		for _, k := range reqs {
			typ, id, _ := strings.Cut(k, ":")
			n, _ := strconv.Atoi(id)
			res = append(res, map[string]any{"createdAt": "2026-11-05T12:00:00Z", "media": map[string]any{"tmdbId": n, "mediaType": typ}})
		}
		mu.Unlock()
		send(w, map[string]any{"pageInfo": map[string]int{"page": 1, "pages": 1}, "results": res})
	case p == "request" && r.Method == http.MethodPost:
		var body struct {
			MediaType  string `json:"mediaType"`
			MediaID    int    `json:"mediaId"`
			ProfileID  int    `json:"profileId"`
			RootFolder string `json:"rootFolder"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		k := fmt.Sprintf("%s:%d", body.MediaType, body.MediaID)
		log.Printf("REQUEST %s as X-API-User=%s profileId=%d rootFolder=%q", k, r.Header.Get("X-API-User"), body.ProfileID, body.RootFolder)
		mu.Lock()
		status[k], reqs = 2, append(reqs, k)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		send(w, map[string]any{"id": 1})
	case len(parts) == 2:
		t, ok := byKey[parts[0]+":"+parts[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			send(w, map[string]string{"message": "Unable to retrieve " + parts[0]})
			return
		}
		send(w, withStatus(t))
	case len(parts) == 3:
		// Deterministic "recommendations": 14 titles of the same type, chosen by the ID.
		id, _ := strconv.Atoi(parts[1])
		list := ofType[parts[0]]
		var res []title
		for i := 0; i < 14 && len(list) > 0; i++ {
			t := list[(id*7+i*13)%len(list)]
			if t.id() != id {
				res = append(res, withStatus(t))
			}
		}
		send(w, map[string]any{"results": res})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func jellyfin(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/Users/u1/Items"):
		send(w, map[string]any{"Items": watched})
	case r.URL.Path == "/Items":
		send(w, map[string]any{"Items": []map[string]any{{"Id": "s1", "ProviderIds": map[string]string{"Tmdb": "70523"}}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
