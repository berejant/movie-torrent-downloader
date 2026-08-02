# Movie Torrent Downloader

Searches one or more torrent trackers for a list of movie titles and saves the
matching `.torrent` files to a directory a download client watches. It never
downloads the movies themselves.

Built for Synology Docker, but nothing in it is Synology-specific.

See [AGENTS.md](AGENTS.md) for the full specification.

## What it does

1. You paste a list of movie titles into a web form (one per line).
2. Each line becomes a queued request.
3. Five workers take the requests. Each one searches **every configured tracker
   in parallel**, ranks the merged results, and saves the winner's `.torrent`
   file as `<title>-<tracker>-<quality>-<requestID>.torrent`.
4. The job table shows every request until it reaches a terminal state, with
   retry, edit-query, force, cancel and remove actions.

Release selection is deterministic: **quality tier** (`2160p` > `1080p` >
`720p` > `sd`), then **codec** (H.265 > H.264), then **tracker priority**, then
**larger file**. The picture wins over its source — a 2160p release on the
second-choice tracker beats a 1080p one on the first — and priority only
separates candidates that are otherwise equal. Seeder counts are ignored on
purpose: cross-posted results carry unreliable swarm numbers.

A tracker that is unreachable does not hold back a release another tracker
found; the failure is logged and the remaining results are ranked as usual.

## Supported trackers

Trackers are configured by slug, and each slug carries a **preset**: the paths,
form fields, selectors and column layout of that site. Adding a supported
tracker means listing its slug and supplying credentials.

| Preset | Site | Notes |
|---|---|---|
| `toloka` | toloka.to | phpBB2 engine; search requires a session |
| `mazepa` | mazepa.to | TorrentPier |
| `torrentpier` | — | the generic engine, for any other TorrentPier install |

```sh
TRACKERS=toloka,mazepa
TRACKER_TOLOKA_LOGIN=…
TRACKER_TOLOKA_PASSWORD=…
TRACKER_TOLOKA_PRIORITY=1
TRACKER_MAZEPA_LOGIN=…
TRACKER_MAZEPA_PASSWORD=…
TRACKER_MAZEPA_PRIORITY=2
```

Any preset value can be corrected without a code change via
`TRACKER_<SLUG>_EXTRA_OPTIONS` (see `.env.example`). Leaving `TRACKERS` unset
falls back to the legacy single-tracker layout on unprefixed `TRACKER_*`
variables, so an existing `.env` keeps working.

## Quick start (Docker Compose)

```sh
cp .env.example .env
# set the tracker credentials, and PUID/PGID to the owner of your shares
docker compose up -d
```

Open <http://localhost:8080>.

## Quick start (docker run)

```sh
docker build -t movie-torrent-downloader .

docker run -d \
  --name movie-torrent-downloader \
  --restart unless-stopped \
  --env-file .env \
  -p 8080:8080 \
  -v /volume1/downloads/torrents:/torrents \
  -v /volume1/docker/mtd/db:/data \
  movie-torrent-downloader
```

### Synology notes

- `PUID`/`PGID` must match the owner of the mounted shares, or the container
  cannot write to them. Find them over SSH with `id your-user`.
- Mount the `.torrent` output at a path your download client watches.
- `/data` holds the SQLite database. Losing it loses request history, not the
  saved files.

## Local development (no Docker)

```sh
cp .env.example .env
# point TORRENT_FILES_DIR and DB_PATH at local directories
go run ./cmd/server
```

`.env` is loaded automatically by godotenv; real environment variables take
precedence over it.

```sh
go test ./...     # ranking, normalization and the tracker pipeline
go vet ./...
```

## CI and published images

`.github/workflows/ci.yml` runs on every push and pull request:

1. **Test** — `gofmt` check, `go vet`, `go test -race`, `go build`.
2. **Docker** — builds `linux/amd64` and `linux/arm64` and pushes to GHCR.

Pull requests build the image but never publish it. Pushes to the default
branch publish `latest`; version tags (`v1.2.3`) publish `1.2.3` and `1.2`.
Every build is also tagged with its short commit SHA.

Pull a published image with:

```sh
docker pull ghcr.io/<owner>/movie-torrent-downloader:latest
```

Packages are private by default — make the package public (or log the NAS in
with a personal access token that has `read:packages`) before pulling from
Synology.

## Configuration

Every setting is an environment variable — see [.env.example](.env.example) for
the annotated list. The ones that matter most:

| Variable | Default | Purpose |
|---|---|---|
| `TORRENT_FILES_DIR` | *required* | where `.torrent` files are written |
| `TRACKERS` | unset | comma-separated tracker slugs; unset = legacy single tracker |
| `TRACKER_<SLUG>_LOGIN` / `_PASSWORD` | *required* | that tracker's credentials |
| `TRACKER_<SLUG>_PRIORITY` | `1` | lower wins; breaks ties between equal releases |
| `TRACKER_<SLUG>_BASE_URL` | from preset | tracker root URL |
| `DB_PATH` | `/data/app.db` | SQLite file |
| `WORKERS` | `5` | requests searched at once, across all trackers |
| `TRACKER_<SLUG>_RPS` | `1` | request rate for that tracker |
| `AUTH_USER` / `AUTH_PASSWORD` | unset | enables basic auth when both are set |
| `PUID` / `PGID` | `1000` | container user, must own the mounts |

## Endpoints

| Path | Purpose |
|---|---|
| `/` | operator UI |
| `/health/live` | process is up |
| `/health/ready` | database open **and** output directory writable |

Readiness deliberately excludes tracker reachability, so a tracker outage does
not restart the container.

## Behaviour worth knowing

- **Duplicates** are rejected only against requests that reached `DOWNLOADED`.
  A rejected line is still saved with status `DUPLICATE` so you can edit its
  query or force it. Failed and not-found titles can be resubmitted freely.
- **Retries** are persisted (`next_attempt_at`), not slept in memory: 5
  attempts, 3s → 6s → 12s → 24s → 48s with jitter, capped at 60s. A restart
  resumes the schedule.
- **Restart** re-queues anything that was mid-flight and deletes orphaned
  temporary files. Tasks are cheap to repeat, so they rerun from the start.
- **Cancel** applies to `NEW` and `QUEUED` only; in-flight work runs to
  completion.
- **The job table stops polling** when nothing is in flight, and pauses while
  rows are selected so a refresh cannot wipe a batch selection.

## Troubleshooting

**Nothing is found and every request fails.** Each preset targets one table
layout. If a tracker changes its markup, override the selectors with
`TRACKER_<SLUG>_EXTRA_OPTIONS` (see `.env.example`) rather than editing code.
Saved copies of the pages the presets were written against live in
`html-examples/`, and the parser tests run against them.

**One tracker stopped contributing.** Look for `tracker unavailable, ranking the
remaining results` in the logs: results from the other trackers are still used,
so requests keep completing while one source is broken.

**Permission denied writing torrents.** `PUID`/`PGID` do not match the share
owner. `/health/ready` reports this directly.

**Login fails.** Check the credentials first; if they are right, the login form
field names may have changed — they are overridable in
`TRACKER_<SLUG>_EXTRA_OPTIONS`, including any extra checkbox the form submits
(`login_extra_fields`).
