# spor

A per-person wall of movies and shows you don't have yet, each "because you
watched X" (or requested X). Click a poster for its summary, a Request button
and its own recommendations; keep clicking to explore. The breadcrumb shows the
trail that led you there. No AI: recommendations are TMDb's, through Seerr.

- **Discover**: recommendations for what you watched (Jellyfin) and requested
  (Seerr), ranked by weight. Never shows what's in the library, on its way,
  watched, requested or blocked. Long-press (or right-click) a poster to block it.
- **Title page**: overview, Request (made in Seerr **as you**, so Seerr's own
  permissions and auto-approval apply), and more like it.
- **Library**: everything in the library; rate what you've watched 👍/👎.
  Unwatched titles are greyed out ("Not watched") and can't be rated.
- **Blocked**: unblock.

spor keeps its own copy of each person's watch history and requests, so a
title deleted after watching (reaparr) still counts.

## Weighting

A seed (watched/requested title) weighs: 👍 +1.5, watched +1.0, requested only
+0.8, 👎 −1.0 (overrides). A recommendation scores the sum of the seeds that
recommend it; ≤ 0 is never shown.

## Configuration

| Variable | |
|---|---|
| `SPOR_SEERR_URL`, `SPOR_SEERR_API_KEY` | Seerr and its API key (admin-level: kept server-side only) |
| `SPOR_JELLYFIN_URL`, `SPOR_JELLYFIN_API_KEY` | Jellyfin and an API key (reads every user's watch history) |
| `SPOR_USER_MAP` | Optional `email=seerr-user-id,...` for logins whose email differs from their Seerr email |
| `SPOR_SYNC_INTERVAL` | How often history is copied from Jellyfin/Seerr. Default `15m`, minimum `1m` |
| `SPOR_OIDC_*`, `SPOR_AUTH_DISABLED`, ... | Login, see [authy](https://github.com/miista/authy). Scopes asked: `openid profile email groups` |
| `SPOR_ANONYMOUS_USER` | Only with `SPOR_AUTH_DISABLED=true` (local trials): the Seerr user everyone acts as |
| `SPOR_DATA_DIR` | Default `/app/data`: `spor.db` (SQLite) and `auth/` |
| `SPOR_ADDR`, `SPOR_LOG_LEVEL` | Default `:8080`, `info` |

A signed-in person is matched to their Seerr account by email, then to Jellyfin
through the Seerr account's linked Jellyfin user.

## One-off backfill

Titles watched *and* deleted before spor's first sync live only in Jellyfin's
database (on `PLACEHOLDER` rows). To import them once, from a **copy**:

```
spor backfill /path/to/copy-of-jellyfin.db   # needs SPOR_SEERR_URL/_API_KEY, SPOR_DATA_DIR
```

Only titles spor knows nothing about are added (safe to re-run). Delete the copy afterwards.

## Develop

```
make test                 # -race -shuffle=on
go run ./hack/fakeback    # fake Seerr :5055 + Jellyfin :8096 (public TMDb data, made-up people)
SPOR_SEERR_URL=http://localhost:5055 SPOR_SEERR_API_KEY=x \
SPOR_JELLYFIN_URL=http://localhost:8096 SPOR_JELLYFIN_API_KEY=x \
SPOR_AUTH_DISABLED=true SPOR_ANONYMOUS_USER=1 SPOR_DATA_DIR=/tmp/spor go run .
make image                # cross-compiles, then builds from the binary
```
