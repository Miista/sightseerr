package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// directory maps a signed-in person to their Seerr account: by the email
// authy reports, unless SPOR_USER_MAP names the Seerr user for that email.
// The Seerr user list is refreshed by the sync.
type directory struct {
	overrides map[string]int // lower-cased email → Seerr user ID

	mu    sync.RWMutex
	users map[int]SeerrUser
}

// parseUserMap reads "alice@example.com=1,bob@example.com=3".
func parseUserMap(s string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		email, id, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(strings.TrimSpace(id))
		email = strings.ToLower(strings.TrimSpace(email))
		if !ok || err != nil || n <= 0 || email == "" {
			return nil, fmt.Errorf("SPOR_USER_MAP: %q is not email=seerr-user-id", part)
		}
		out[email] = n
	}
	return out, nil
}

func newDirectory(overrides map[string]int) *directory {
	return &directory{overrides: overrides, users: map[int]SeerrUser{}}
}

func (d *directory) set(users []SeerrUser) {
	m := make(map[int]SeerrUser, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	d.mu.Lock()
	d.users = m
	d.mu.Unlock()
}

func (d *directory) all() []SeerrUser {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]SeerrUser, 0, len(d.users))
	for _, u := range d.users {
		out = append(out, u)
	}
	return out
}

func (d *directory) byID(id int) (SeerrUser, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	u, ok := d.users[id]
	return u, ok
}

// resolve finds the Seerr user for an email. An empty email never matches:
// "no email" must not land on some account whose email is also empty.
func (d *directory) resolve(email string) (SeerrUser, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return SeerrUser{}, false
	}
	if id, ok := d.overrides[email]; ok {
		return d.byID(id)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var found SeerrUser
	matches := 0
	for _, u := range d.users {
		if strings.EqualFold(strings.TrimSpace(u.Email), email) {
			found = u
			matches++
		}
	}
	// Two Seerr accounts with the same email: refuse rather than guess.
	return found, matches == 1
}
