package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/miista/authy"
)

// version is stamped at build time (-X main.version=...).
var version = "dev"

// appName sets the authy variables (SIGHTSEERR_OIDC_*).
const (
	appName        = "sightseerr"
	defaultAddr    = ":8080"
	defaultDataDir = "/app/data"
	defaultSync    = 15 * time.Minute
)

//go:embed web/static
var staticFiles embed.FS

// config is everything sightseerr reads from the environment.
type config struct {
	seerrURL, seerrKey       string
	seerrPublic              string
	jellyfinURL, jellyfinKey string
	userMap                  map[string]int
	anonymous                int
	dryRun                   bool
	syncEvery                time.Duration
	addr, dataDir            string
}

func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		seerrURL: getenv("SIGHTSEERR_SEERR_URL"), seerrKey: getenv("SIGHTSEERR_SEERR_API_KEY"),
		jellyfinURL: getenv("SIGHTSEERR_JELLYFIN_URL"), jellyfinKey: getenv("SIGHTSEERR_JELLYFIN_API_KEY"),
		syncEvery: defaultSync, addr: defaultAddr, dataDir: defaultDataDir,
	}
	var missing []string
	for name, v := range map[string]string{
		"SIGHTSEERR_SEERR_URL": c.seerrURL, "SIGHTSEERR_SEERR_API_KEY": c.seerrKey,
		"SIGHTSEERR_JELLYFIN_URL": c.jellyfinURL, "SIGHTSEERR_JELLYFIN_API_KEY": c.jellyfinKey,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return c, fmt.Errorf("missing settings: %s", strings.Join(missing, ", "))
	}
	var err error
	// Links in the page go to Seerr as the browser reaches it, which can
	// differ from how sightseerr reaches it (an internal Docker name).
	c.seerrPublic = strings.TrimRight(cmp.Or(getenv("SIGHTSEERR_SEERR_PUBLIC_URL"), c.seerrURL), "/")
	if u, perr := url.Parse(c.seerrPublic); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("SIGHTSEERR_SEERR_PUBLIC_URL (or SIGHTSEERR_SEERR_URL) must be an http(s) URL, not %q", c.seerrPublic)
	}
	if c.userMap, err = parseUserMap(getenv("SIGHTSEERR_USER_MAP")); err != nil {
		return c, err
	}
	if v := getenv("SIGHTSEERR_ANONYMOUS_USER"); v != "" {
		if c.anonymous, err = strconv.Atoi(v); err != nil || c.anonymous <= 0 {
			return c, fmt.Errorf("SIGHTSEERR_ANONYMOUS_USER must be a Seerr user ID, not %q", v)
		}
	}
	if v := getenv("SIGHTSEERR_REQUESTS_DRY_RUN"); v != "" {
		if c.dryRun, err = strconv.ParseBool(v); err != nil {
			return c, fmt.Errorf("SIGHTSEERR_REQUESTS_DRY_RUN must be true or false, not %q", v)
		}
	}
	if v := getenv("SIGHTSEERR_SYNC_INTERVAL"); v != "" {
		if c.syncEvery, err = time.ParseDuration(v); err != nil || c.syncEvery < time.Minute {
			return c, fmt.Errorf("SIGHTSEERR_SYNC_INTERVAL must be a duration of at least 1m, not %q", v)
		}
	}
	if v := getenv("SIGHTSEERR_ADDR"); v != "" {
		c.addr = v
	}
	if v := getenv("SIGHTSEERR_DATA_DIR"); v != "" {
		c.dataDir = v
	}
	return c, nil
}

func main() {
	if handled, code := command(os.Args[1:], os.Getenv, os.Stdout); handled {
		os.Exit(code)
	}
	logger := newLogger(os.Getenv("SIGHTSEERR_LOG_LEVEL"))
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Fatal().Msg(err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath, err := storePath(cfg.dataDir)
	if err != nil {
		logger.Fatal().Msg(err.Error())
	}
	store, err := openStore(dbPath)
	if err != nil {
		logger.Fatal().Msg(err.Error())
	}
	defer store.Close()

	// Login. authy is on unless SIGHTSEERR_AUTH_DISABLED=true: with no SIGHTSEERR_OIDC_* the
	// first visitor creates a local account; with them the identity provider
	// signs people in. sightseerr needs the email (to find the person's Seerr
	// account); groups is asked for only because authy reads UserInfo, where
	// Authelia serves the profile and email, only when groups were requested.
	gate, err := authy.FromEnv(ctx, appName, authy.WithDataDir(filepath.Join(cfg.dataDir, "auth")),
		authy.WithScopes("openid", "profile", "email", "groups"),
		authy.WithPublicPaths("/favicon.svg"))
	if err != nil {
		logger.Fatal().Err(err).Msg("login is misconfigured")
	}
	if gate == nil {
		if cfg.anonymous == 0 {
			logger.Warn().Msg("login is OFF (SIGHTSEERR_AUTH_DISABLED) and SIGHTSEERR_ANONYMOUS_USER is unset: every page will refuse")
		} else {
			logger.Warn().Msg(fmt.Sprintf("login is OFF (SIGHTSEERR_AUTH_DISABLED): anyone who can reach this port acts as Seerr user %d", cfg.anonymous))
		}
	}

	seerr := newCachedSeerr(newSeerrClient(cfg.seerrURL, cfg.seerrKey))
	people := newDirectory(cfg.userMap)
	statuses := &statusBook{}
	sy := &syncer{
		seerr: seerr, jellyfin: newJellyfinClient(cfg.jellyfinURL, cfg.jellyfinKey),
		store: store, people: people, statuses: statuses, log: withComponent(logger, "sync"),
	}
	go sy.run(ctx, cfg.syncEvery)

	static, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		logger.Fatal().Err(err).Msg("embedded frontend assets missing")
	}
	srv := &Server{
		seerr: seerr, images: newImageStore(filepath.Join(cfg.dataDir, "images")),
		store: store, people: people, statuses: statuses, ready: sy.isReady,
		static: static, log: logger, now: time.Now, dryRun: cfg.dryRun, seerrPublic: cfg.seerrPublic,
	}
	if cfg.dryRun {
		logger.Warn().Msg("requests are a DRY RUN (SIGHTSEERR_REQUESTS_DRY_RUN): nothing is sent to Seerr")
	}
	if gate == nil {
		srv.anonymous = cfg.anonymous // with a login, nobody is anonymous
	}
	var handler http.Handler = srv.Handler()
	if gate != nil {
		handler = gate.Wrap(handler)
	}
	server := &http.Server{Addr: cfg.addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	logger.Info().Msg(fmt.Sprintf("starting %s %s on %s, syncing every %s", appName, version, cfg.addr, cfg.syncEvery))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal().Err(err).Msg("server stopped")
	}
}

// command handles the subcommands: version, healthcheck (the image has no
// curl, so the binary probes its own /healthz) and backfill.
func command(args []string, getenv func(string) string, out io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(out, version)
		return true, 0
	case "healthcheck":
		addr := getenv("SIGHTSEERR_ADDR")
		if addr == "" {
			addr = defaultAddr
		}
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + addr + "/healthz")
		if err != nil {
			fmt.Fprintln(out, "unhealthy:", err)
			return true, 1
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintln(out, "unhealthy: status", resp.StatusCode)
			return true, 1
		}
		return true, 0
	case "backfill":
		if len(args) != 2 {
			fmt.Fprintln(out, "usage: sightseerr backfill <copy of jellyfin.db>")
			return true, 2
		}
		if err := runBackfill(context.Background(), args[1], getenv, out); err != nil {
			fmt.Fprintln(out, "backfill failed:", err)
			return true, 1
		}
		return true, 0
	}
	return false, 0
}
