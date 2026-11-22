# spor

A per-person wall of movies and shows you don't have yet, each "because you
watched X" (or requested or starred X). Click a poster for its summary, a
Request button, a watchlist star and its own recommendations; keep clicking to
explore. The breadcrumb shows the trail that led you there. No AI:
recommendations are TMDb's, through Seerr.

- **Discover**: recommendations for what you watched (Jellyfin), requested and
  starred (Seerr), ranked by weight (see [Weighting](#weighting)). Never shows
  what's in the library, on its way, watched, requested, starred or blocked.
  Long-press (or right-click) a poster to block it.
- **Title page**: overview, Rotten Tomatoes / IMDb / TMDB scores (from Seerr),
  Request (made in Seerr **as you**, so Seerr's own permissions and
  auto-approval apply; quality profile and root folder for those Seerr lets
  choose), the ☆ star (your Seerr watchlist, also as you), the collection it
  belongs to, and more like it. The **Path** lists the titles you tapped
  through, so you can jump back (and forward again).
- **Library**, in four sections:
  - **Watched**: in the library and seen; rate it 👍/👎.
  - **Not watched**: in the library, not seen yet; greyed out, can't be rated.
  - **Removed**: watched, then deleted from the library (reaparr); still rateable.
  - **Watchlist**: your starred titles; remove the star here.
- **Blocked**: unblock.

spor keeps its own copy of each person's watch history and requests, so a
title deleted after watching (reaparr) still counts.

## Weighting

Each of your titles (a seed) counts with its strongest signal:

| Signal | Weight | |
|---|---|---|
| 👍 liked | +1.5 | rated in Library |
| watched | +1.0 | played in Jellyfin |
| requested | +0.8 | requested in Seerr ("get this now") |
| starred | +0.6 | on your Seerr watchlist ("maybe later") |
| 👎 disliked | −1.0 | overrides everything else |

A recommendation scores the sum of the weights of the seeds that recommend it;
≤ 0 is never shown. A starred title is hidden from Discover (find it in
Library → Watchlist) but stays in a title page's "More like this", marked with
a star. The ⓘ on a title page ("why this score") shows the sum.

## Configuration

| Variable | |
|---|---|
| `SPOR_SEERR_URL`, `SPOR_SEERR_API_KEY` | Seerr and its API key (admin-level: kept server-side only) |
| `SPOR_JELLYFIN_URL`, `SPOR_JELLYFIN_API_KEY` | Jellyfin and an API key (reads every user's watch history) |
| `SPOR_USER_MAP` | Optional `email=seerr-user-id,...` for logins whose email differs from their Seerr email |
| `SPOR_SYNC_INTERVAL` | How often history is copied from Jellyfin/Seerr. Default `15m`, minimum `1m` |
| `SPOR_OIDC_*`, `SPOR_AUTH_DISABLED`, ... | Login, see [authy](https://github.com/miista/authy). Scopes asked: `openid profile email groups` |
| `SPOR_ANONYMOUS_USER` | Only with `SPOR_AUTH_DISABLED=true` (local trials): the Seerr user everyone acts as |
| `SPOR_DATA_DIR` | Default `/app/data`: `spor.db` (SQLite), `auth/`, and `images/`, the posters spor serves so the page never contacts TMDb (a cache: safe to delete, not worth backing up) |
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
