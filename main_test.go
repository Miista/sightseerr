package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func required() map[string]string {
	return map[string]string{
		"SPOR_SEERR_URL": "http://seerr:5055", "SPOR_SEERR_API_KEY": "s",
		"SPOR_JELLYFIN_URL": "http://jellyfin:8096", "SPOR_JELLYFIN_API_KEY": "j",
	}
}

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(envOf(required()))
	if err != nil || c.syncEvery != defaultSync || c.addr != defaultAddr || c.dataDir != defaultDataDir || c.anonymous != 0 {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	if c.seerrPublic != "http://seerr:5055" {
		t.Errorf("public Seerr URL defaults to %q, want SPOR_SEERR_URL", c.seerrPublic)
	}
	env := required()
	env["SPOR_SEERR_PUBLIC_URL"] = "https://seerr.example.com/"
	if c, _ := loadConfig(envOf(env)); c.seerrPublic != "https://seerr.example.com" {
		t.Errorf("public Seerr URL %q", c.seerrPublic)
	}
	env["SPOR_SYNC_INTERVAL"], env["SPOR_ANONYMOUS_USER"], env["SPOR_USER_MAP"] = "5m", "3", "a@example.com=1"
	if c, err := loadConfig(envOf(env)); err != nil || c.syncEvery != 5*time.Minute || c.anonymous != 3 || c.userMap["a@example.com"] != 1 {
		t.Fatalf("set: %+v, %v", c, err)
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	_, err := loadConfig(envOf(map[string]string{"SPOR_SEERR_URL": "x"}))
	if err == nil || !strings.Contains(err.Error(), "SPOR_JELLYFIN_API_KEY") || strings.Contains(err.Error(), "SPOR_SEERR_URL") {
		t.Fatalf("missing settings error: %v", err)
	}
	for name, v := range map[string]string{
		"SPOR_SYNC_INTERVAL":    "10s",
		"SPOR_REQUESTS_DRY_RUN": "maybe",
		"SPOR_ANONYMOUS_USER":   "bob",
		"SPOR_USER_MAP":         "nonsense",
		"SPOR_SEERR_PUBLIC_URL": "javascript:alert(1)",
	} {
		env := required()
		env[name] = v
		if _, err := loadConfig(envOf(env)); err == nil {
			t.Errorf("%s=%q accepted", name, v)
		}
	}
}

func TestCommands(t *testing.T) {
	var out bytes.Buffer
	if handled, code := command([]string{"version"}, envOf(nil), &out); !handled || code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %v %d %q", handled, code, out.String())
	}
	if handled, _ := command(nil, envOf(nil), &out); handled {
		t.Fatal("no arguments handled as a command")
	}
	out.Reset()
	if handled, code := command([]string{"backfill"}, envOf(nil), &out); !handled || code != 2 {
		t.Fatalf("backfill without a path: %v %d", handled, code)
	}
	// Nothing listens on this port: unhealthy.
	if handled, code := command([]string{"healthcheck"}, envOf(map[string]string{"SPOR_ADDR": "127.0.0.1:1"}), &out); !handled || code != 1 {
		t.Fatalf("healthcheck against nothing: %v %d", handled, code)
	}
}
