# Movie Torrent Downloader

Searches a torrent tracker for a list of movie titles and saves the matching
`.torrent` files to a directory a download client watches. It never downloads
the movies themselves.

Built for Synology Docker, but nothing in it is Synology-specific.

See [AGENTS.md](AGENTS.md) for the full specification.

## What it does

1. You paste a list of movie titles into a web form (one per line).
2. Each line becomes a queued request.
3. Five workers search the tracker, pick the best release and save its
   `.torrent` file as `<title>-<tracker>-<quality>-<requestID>.torrent`.
4. The job table shows every request until it reaches a terminal state, with
   retry, edit-query, force, cancel and remove actions.

Release selection is deterministic: **quality tier** (`2160p` > `1080p` >
`720p` > `sd`), then **codec** (H.265 > H.264), then **larger file**. Seeder
counts are ignored on purpose — cross-posted results carry unreliable swarm
numbers.

## Quick start (Docker Compose)

```sh
cp .env.example .env
# set TRACKER_LOGIN / TRACKER_PASSWORD, and PUID/PGID to the owner of your shares
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

## Configuration

Every setting is an environment variable — see [.env.example](.env.example) for
the annotated list. The ones that matter most:

| Variable | Default | Purpose |
|---|---|---|
| `TORRENT_FILES_DIR` | *required* | where `.torrent` files are written |
| `TRACKER_BASE_URL` | *required* | tracker root URL |
| `TRACKER_LOGIN` / `TRACKER_PASSWORD` | *required* | tracker credentials |
| `DB_PATH` | `/data/app.db` | SQLite file |
| `TRACKER_WORKERS` | `5` | concurrent workers |
| `TRACKER_RPS` | `1` | shared request rate across all workers |
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

**Nothing is found and every request fails.** The parser targets TorrentPier's
table layout. If the tracker changes its markup, override the selectors with
`TRACKER_EXTRA_OPTIONS` (see `.env.example`) rather than editing code.

**Permission denied writing torrents.** `PUID`/`PGID` do not match the share
owner. `/health/ready` reports this directly.

**Login fails.** Check the credentials first; if they are right, the login form
field names may have changed — they are overridable in `TRACKER_EXTRA_OPTIONS`.
