package main

import "testing"

func TestParseUserMap(t *testing.T) {
	m, err := parseUserMap(" Alice@Example.com=1, bob@example.com = 3 ,")
	if err != nil || m["alice@example.com"] != 1 || m["bob@example.com"] != 3 || len(m) != 2 {
		t.Fatalf("got %v, %v", m, err)
	}
	for _, bad := range []string{"alice@example.com", "alice@example.com=x", "=3", "alice@example.com=0", "alice@example.com=-1"} {
		if _, err := parseUserMap(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	d := newDirectory(map[string]int{"carol@example.com": 3})
	d.set([]SeerrUser{
		{ID: 1, Email: "Alice@Example.com"},
		{ID: 3, Email: "something-else@example.com"},
		{ID: 4, Email: ""},
		{ID: 5, Email: "twin@example.com"},
		{ID: 6, Email: "twin@example.com"},
	})
	cases := []struct {
		email  string
		wantID int // 0: must not match
	}{
		{"alice@example.com", 1},
		{" ALICE@example.com ", 1},
		{"carol@example.com", 3}, // override
		{"", 0},                  // never matches the account with no email
		{"nobody@example.com", 0},
		{"twin@example.com", 0}, // ambiguous: refuse rather than guess
	}
	for _, c := range cases {
		u, ok := d.resolve(c.email)
		if c.wantID == 0 && ok {
			t.Errorf("resolve(%q) matched user %d, want no match", c.email, u.ID)
		}
		if c.wantID != 0 && (!ok || u.ID != c.wantID) {
			t.Errorf("resolve(%q) = %d, %v; want %d", c.email, u.ID, ok, c.wantID)
		}
	}
}

func TestResolveOverrideToMissingUser(t *testing.T) {
	d := newDirectory(map[string]int{"alice@example.com": 99})
	d.set([]SeerrUser{{ID: 1, Email: "alice@example.com"}})
	// The override wins even when it points nowhere: an operator's explicit
	// mapping must not quietly fall back to a guess by email.
	if u, ok := d.resolve("alice@example.com"); ok {
		t.Fatalf("matched user %d through a broken override", u.ID)
	}
}
