# AGENTS.md

## Project: Movie Torrent Downloader

### Goal
Build a utility service that searches torrent files for movies on a configured tracker and saves `.torrent` files for later use.

The service must run in Docker (primary target: Synology Docker), while remaining platform-agnostic.

> This document is the **source of truth**. Where `mazepa-torrent-download.go` disagrees with it, the prototype is wrong and must be updated. The prototype is an unfinished design sketch (it does not compile: `colly` is missing from the import block, `min` is redeclared against the Go builtin, and there is no `go.mod`).

---

## Tech Stack

| Concern | Choice |
|---|---|
| Language | Go |
| HTTP framework | Echo |
| Database | SQLite (`modernc.org/sqlite`, pure Go — no CGO, so the image stays small and cross-builds cleanly) |
| Scraping | `net/http` + `goquery` over a shared cookie-jar client (**not** `colly` — see note) |
| Templating | `html/template`, templates and static assets embedded via `embed.FS` |
| Frontend | htmx + Pico.css (classless), optional Alpine.js for selection state |
| Config | `github.com/caarlos0/env/v10` for binding env vars into structs |
| Env file loading | `github.com/joho/godotenv` — loads `.env` for local dev runs outside Docker |
| Logging | `log/slog`, JSON handler |

No Node build step. No SPA. Everything ships as one static binary plus embedded assets.

**Why not colly.** The original stack decision named `colly`, and the prototype used it. During implementation it turned out to work against every requirement in §4 and §7: colly owns request headers (via `OnRequest`), owns rate limiting (`LimitRule`), and owns concurrency (its own parallelism), while this service needs to control all three itself — a precise browser header set with a Referer chain, one shared token bucket across five workers, and singleflight login. None of colly's actual value (crawling, link following, queueing, caching) is used here, since every request is a single known URL. The result is `net/http` with a shared cookie jar plus `goquery` for parsing, which is fewer moving parts and total control over the wire format.

---

## Core Requirements

### 1) Tracker Search Utility
- Find torrent files for movie titles from a configured torrent tracker.
- Support tracker-specific auth and request options.
- Save selected `.torrent` files to a configured directory.
- **MVP ships a single tracker: `mazepa.to`.** The code must sit behind a `Tracker` interface with a configured priority value so additional trackers (rutracker, etc.) can be added later without restructuring, but only one is wired up now.

**mazepa.to runs TorrentPier** (confirmed). The row parser targets the TorrentPier tracker-table column layout: publish, status, forum, topic, author, size/download-link, seeders, leechers, replies, added.

### 2) Docker-First Runtime
- Must run as a containerized app.
- Compatible with Synology Docker (volume mounts, env-file config, non-root).
- Avoid host-specific assumptions so it can run on any Linux-compatible Docker host.
- The container runs as a **non-root** user whose UID/GID are set from `PUID`/`PGID` at startup (entrypoint adjusts the runtime user and `chown`s the data/output dirs). Without this, Synology volume mounts are not writable.

### 3) Configuration via Environment Variables / Env File

#### Application
| Variable | Default | Notes |
|---|---|---|
| `HTTP_PORT` | `8080` | web server listening port |
| `TORRENT_FILES_DIR` | — | **required**, path to store downloaded `.torrent` files |
| `DB_PATH` | `/data/app.db` | SQLite file, must live on a mounted volume |
| `TZ` | `UTC` | display timezone; all timestamps are **stored in UTC** |
| `PUID` / `PGID` | `1000` / `1000` | runtime user for non-root operation |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `BATCH_MAX_LINES` | `100` | max movie titles per batch submission |
| `AUTH_USER` / `AUTH_PASSWORD` | unset | HTTP Basic auth for UI + API. Enabled **only when both are set**; unset means no auth (LAN-only deployments) |

#### Tracker (prefix `TRACKER_`)
| Variable | Default | Notes |
|---|---|---|
| `TRACKER_NAME` | `mazepa` | short slug, used in saved filenames |
| `TRACKER_BASE_URL` | — | **required** |
| `TRACKER_LOGIN` | — | **required** |
| `TRACKER_PASSWORD` | — | **required** |
| `TRACKER_PRIORITY` | `1` | lower wins when multiple trackers exist (future use) |
| `TRACKER_TIMEOUT_SECONDS` | `30` | per-request timeout |
| `TRACKER_WORKERS` | `5` | concurrent workers for this tracker |
| `TRACKER_RPS` | `1` | shared rate limit across all workers of this tracker |
| `TRACKER_MAX_SIZE_BYTES` | `0` | `0` = unlimited |
| `TRACKER_USER_AGENT` | real browser UA (see below) | overridable |
| `TRACKER_EXTRA_OPTIONS` | unset | JSON object of tracker-specific knobs (see below) |

`TRACKER_EXTRA_OPTIONS` is a JSON object; every key is optional and falls back to the TorrentPier defaults:

```json
{
  "tracker_path": "/tracker.php",
  "login_path": "/login.php",
  "login_username_field": "login_username",
  "login_password_field": "login_password",
  "login_submit_field": "login",
  "login_submit_value": "Увійти",
  "search_query_field": "nm",
  "logged_in_selector": "a[href*='logout']",
  "logged_out_selector": "#register_link",
  "result_row_selector": "#forum_table tbody tr",
  "topic_link_selector": "a[href*='topic-']",
  "download_link_selector": "a[href*='dl.php?id=']",
  "forum_link_selector": "a[href*='forum-']"
}
```

#### Retry
| Variable | Default |
|---|---|
| `RETRY_MAX_ATTEMPTS` | `5` |
| `RETRY_BASE_SECONDS` | `3` |
| `RETRY_MAX_BACKOFF_SECONDS` | `60` |

#### Duplicates
| Variable | Default | Notes |
|---|---|---|
| `DUPLICATE_CHECK_ENABLED` | `true` | set `false` to globally disable uniqueness checks |

Configuration behavior:
- Validate required env vars on startup.
- Fail fast with clear error messages if required config is missing/invalid.
- Never log secrets (`TRACKER_PASSWORD`, `AUTH_PASSWORD`, tokens, cookies).

**Local development without Docker:** on startup the app calls `godotenv.Load()` before binding config, so a `.env` file in the working directory populates the environment for a plain `go run .`. Loading is best-effort — a missing `.env` is **not** an error, since in Docker the values come from the env-file/compose environment instead. Real environment variables always win over `.env` entries (`godotenv.Load` does not overwrite what is already set). Ship a `.env.example` documenting every variable above with safe placeholder values, and keep `.env` out of version control.

### 4) HTTP Client Behavior

All tracker requests must look like a real browser.

- **User-Agent:** a current desktop Chrome string, e.g.
  `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36`
- **Page requests** send:
  - `Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8`
  - `Accept-Language: uk-UA,uk;q=0.9,ru;q=0.8,en-US;q=0.7,en;q=0.6`
  - `Upgrade-Insecure-Requests: 1`
  - `Sec-Fetch-Dest: document`, `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Site: same-origin`
  - `Referer` set to the plausible previous page (login form → login POST, search page → topic, topic → file download)
- **`.torrent` download** sends `Accept: application/x-bittorrent,*/*;q=0.8` and `Referer: <topic URL>`.
- **Do not set `Accept-Encoding` manually.** Go's transport adds gzip and transparently decompresses only when it owns the header; setting it by hand means handling decompression yourself.
- One cookie jar per tracker, shared by login/search/download so the session is reused.

### 5) Web Interface for Batch Scheduling

Server-rendered HTML fragments driven by htmx. No JSON API is required for the UI (a machine API remains post-MVP).

Provide a web UI to schedule torrent-search jobs:
- Textarea input for batch requests (copy/paste movie list, one per line).
- Submit creates queued jobs.
- Show job list with statuses and basic metadata.

**Batch input rules:**
- Max `BATCH_MAX_LINES` (100) lines per submission; reject the whole batch with a clear message if exceeded.
- Trim each line; drop empty lines.
- Deduplicate identical normalized queries **within the batch** before creating tasks.

Job list actions:
- Retry failed request manually.
- For `NOT_FOUND`, allow editing search query and retry.
- For `DUPLICATE`, allow editing search query and/or forcing the retry.
- Cancel a task that has not started yet.
- Remove task from list.
- Remove related downloaded `.torrent` file when removing task (if file exists).
- Force processing of duplicate task (default duplicate behavior is reject).
- Group/batch actions for selected tasks.

**Refresh strategy — polling, not SSE:**
- The job table is an htmx fragment polled via `hx-get="/jobs/table" hx-trigger="every 3s"`.
- **Pause polling while any row checkbox is selected**, so a batch selection is not wiped by a swap.
- **Emit no polling trigger when every job is in a terminal state** (`DOWNLOADED`, `NOT_FOUND`, `FAILED`, `CANCELLED`, `DUPLICATE`), so an idle page goes quiet. A submit or row action re-arms polling.

Minimum UI flows:
- Create batch request
- View queue/history
- Check status per movie request
- Retry failed request
- Edit query and retry for `NOT_FOUND`
- Edit query and/or force retry for `DUPLICATE`
- Cancel a queued task
- Remove task and related file
- Force duplicate request
- Run batch actions for selected tasks

### 6) Request Processing Policies

#### Query normalization
The normalized query is used for duplicate detection and is derived as:
1. Unicode NFC normalize
2. Lowercase
3. Strip punctuation
4. Collapse internal whitespace runs to a single space
5. Trim

#### Duplicate handling policy
- A request is a duplicate when its normalized query matches an existing request that reached **`DOWNLOADED`**. Requests in `NOT_FOUND`, `FAILED`, `CANCELLED`, or `DUPLICATE` are **ignored** by the check — the same title can be resubmitted freely after a failure.
- A duplicate is **always persisted as a task** with status `DUPLICATE`, never silently discarded. It can then be edited and/or forced from the list.
- UI must expose an explicit `Force` action to bypass the check for a specific request.
- `DUPLICATE_CHECK_ENABLED=false` disables the check globally.

#### Matching policy
Selection is deterministic and **ignores seeders entirely** — results are frequently cross-posted from other trackers with wrong or stale swarm counts. There is **no minimum-seeders filter** and **no language preference**; tracker priority replaces language ranking.

Candidates are ordered by, in strict precedence:
1. **Tracker priority** (`TRACKER_PRIORITY`, lower first) — single tracker in MVP, so inert for now
2. **Quality tier** — `2160p` > `1080p` > `720p` > `sd`
3. **Codec** — H.265/HEVC > H.264/AVC > anything else (better compression at equal size)
4. **Larger `SizeBytes`** (proxy for bitrate, still bounded by `TRACKER_MAX_SIZE_BYTES`)
5. **First-seen order** (stable sort, so ties are reproducible)

**Canonical quality token** — parsed from the release title and normalized, because it is embedded in the saved filename:

| Token | Matches (case-insensitive) |
|---|---|
| `2160p` | `2160p`, `4k`, `uhd` |
| `1080p` | `1080p`, `fullhd`, `full hd` |
| `720p` | `720p`, `hd` |
| `sd` | everything else |

**Canonical codec token:** `h265` (`h.265`, `h265`, `x265`, `hevc`), `h264` (`h.264`, `h264`, `x264`, `avc`), otherwise `other`.

#### Saved filename
```
<title>-<tracker>-<quality>-<requestID>.torrent
```
e.g. `dune part two-mazepa-2160p-01JQ8X4M7ZK3RN.torrent`

- `<title>` is the cleaned release title: letters, digits, `-` and `.` are kept, **every other character becomes a space**, whitespace runs collapse, and only the **first 7 words** survive (140-char backstop). Tracker titles carry the whole release description, and everything past the first few words is either noise or already captured by the quality token:

  ```
  Сікаріо 2 / Sicario: Day of the Soldado (2018) UHD BDRemux 4K 2160p HDR 2xUkr/Eng | Sub Eng
  -> Сікаріо 2 Sicario Day of the Soldado-mazepa-2160p-01KYYJA6CMF8N3Q77NAM0VJ4DQ.torrent
  ```
- `<tracker>` is `TRACKER_NAME`.
- `<requestID>` is the task's ULID — sortable, filename-safe, and **guarantees one file per task**, so removing a task never deletes another task's file.
- Write to a temp file in the same directory, then `os.Rename` into place.
- Deletion on task removal must verify the resolved path is inside `TORRENT_FILES_DIR` before unlinking.

### 7) Concurrency and Rate Limiting
- **Up to 5 workers per tracker** (`TRACKER_WORKERS`).
- All workers of a tracker share **one rate limiter** (token bucket at `TRACKER_RPS`) and **one cookie session**. The limiter, not per-request sleeps, is what protects the tracker.
- The tracker client must **not** hold a global mutex across search/download — that would serialize the 5 workers into 1. Only session establishment is guarded, via singleflight, so five workers hitting an expired session trigger exactly one re-login.
- Per-request timeout via `TRACKER_TIMEOUT_SECONDS` and `context.Context`.

### 8) Retry Policy
- Max 5 attempts (`RETRY_MAX_ATTEMPTS`).
- Exponential backoff with jitter, starting at 3s: 3s → 6s → 12s → 24s → 48s, capped at `RETRY_MAX_BACKOFF_SECONDS` (60s).
- Retries are **persisted**, not slept in memory: the task row carries `attempt_count` and `next_attempt_at`, and the worker picks up due tasks. This is what makes retries survive a restart.
- **Retryable:** network errors, timeouts, HTTP 5xx, HTTP 429. Auth failure triggers one re-login, then fails.
- **Not retryable:** `NOT_FOUND` (no results is an answer, not a failure).

### 9) Persistent Storage for Requests/Statuses
Store movie search requests and lifecycle state in SQLite.

Minimum data to keep:
- Request ID (ULID)
- Movie title (raw input)
- Normalized query
- Current status
- Created/updated timestamps (UTC)
- Last error (if any)
- Attempt count, `next_attempt_at`
- Tracker name
- Tracker result metadata (matched torrent name, size, quality token, codec token, topic URL)
- Saved file path (if downloaded)
- Batch ID (groups the tasks created by one submission)

Statuses:

| Status | Meaning | Terminal |
|---|---|---|
| `NEW` | created, not yet queued | no |
| `QUEUED` | waiting for a worker (includes waiting on `next_attempt_at`) | no |
| `SEARCHING` | worker is querying the tracker | no |
| `FOUND` | matching topic selected, `.torrent` not yet fetched — a **transition step**, never a resting state | no |
| `DOWNLOADED` | `.torrent` saved to `TORRENT_FILES_DIR` | yes |
| `NOT_FOUND` | tracker returned no usable match | yes |
| `FAILED` | retries exhausted | yes |
| `DUPLICATE` | rejected by the uniqueness check | yes |
| `CANCELLED` | cancelled by the operator before it started | yes |

Cancellation: allowed from `NEW` and `QUEUED` only. **In-flight tasks (`SEARCHING`/`FOUND`) are not cancellable** — they run to completion.

---

## Non-Functional Requirements

### Reliability
- Idempotent scheduling support (avoid duplicate processing of identical request unless explicitly allowed).
- **Graceful restart:** on startup, every task in `SEARCHING` or `FOUND` is re-queued to `QUEUED` and executed again from the beginning. Tasks are short and cheap to repeat, so there is no mid-task resume. Orphaned temp files in `TORRENT_FILES_DIR` are cleaned at startup.
- Manual retry must be available from UI list for failed jobs.
- For `NOT_FOUND`, operator must be able to edit query and retry from UI list.

### Performance / Rate-Limiting
See §7. Configurable worker concurrency, shared per-tracker throttle, per-request timeouts.

### Security
- Do not expose credentials in logs or UI.
- HTTP Basic auth for UI/API, enabled when `AUTH_USER` and `AUTH_PASSWORD` are both set.
- Validate/sanitize user input from batch textarea (line count, trimming, normalization).
- Handle tracker sessions/cookies securely; cookie jar stays in memory, never persisted to disk or logged.
- Path-traversal guard on any file deletion.

### Observability
- Structured logs (`slog`, JSON) with request IDs.
- Health endpoints:
    - `/health/live` — process is up
    - `/health/ready` — SQLite is open **and** `TORRENT_FILES_DIR` is writable. **Tracker reachability/login is deliberately excluded** — a tracker outage must not kill the container.
- Optional metrics endpoint (Prometheus format preferred).

### Portability / Operations
- Container runs with mounted volumes for torrent output and the SQLite DB.
- Timezone configurable via `TZ`; storage is always UTC.
- Provide sample `.env.example`.
- Provide explicit Docker run / compose examples.
- DB backup/restore is not required for MVP; potential DB loss is acceptable.
- Anti-bot/CAPTCHA bypass strategies are out of scope for now.

### Testing
No formal test suite is required for MVP. This is a self-hosted personal tool, validated by manual use; pinning external tracker HTML in fixtures is not worth the upkeep. Pure functions (query normalization, quality/codec parsing, ranking, size parsing) may get lightweight table-driven tests where convenient.

---

## Suggested Technical Scope (MVP)

### MVP includes
- Single tracker integration (`mazepa.to`)
- Batch input UI (htmx, polled table)
- Queue + 5 workers behind a shared rate limiter
- Persistent status tracking in SQLite with DB-backed retries
- Save `.torrent` files to configured dir
- Docker image + env-based config + PUID/PGID

### Post-MVP (nice to have)
- Multiple trackers with priority-based fallback
- JSON API endpoints for automation
- Notification hooks (email/Telegram/webhook)
- Manual selection among search results
- History retention/cleanup policy

---

## Acceptance Criteria

- Given valid tracker credentials and movie list, user can submit batch request via web UI.
- Each movie request receives a persisted status transition until terminal state.
- Duplicate requests are persisted as `DUPLICATE`, with explicit `Force` and edit-query actions to override.
- Search ranking follows tracker priority → quality (`2160p`/`1080p`/`720p`/`sd`) → codec (H.265 > H.264) → larger size, ignoring seeders.
- For `NOT_FOUND`, user can edit search query and retry from request list.
- User can cancel a queued task, remove a task, and delete its related `.torrent` file.
- User can execute batch actions for selected tasks.
- Downloaded `.torrent` files appear in `TORRENT_FILES_DIR` named `<title>-<tracker>-<quality>-<requestID>.torrent`.
- Failed requests retry up to 5 times with exponential backoff, surviving a container restart.
- Service runs correctly in Docker as a non-root user with only env-file and mounted volumes.
- Restart re-queues in-flight tasks and does not lose request history or corrupt queue state.

---

## Out of Scope (for now)
- Downloading actual movie content via BitTorrent client.
- Media library management.
- Advanced user/role management.
- Automated tests against live tracker HTML.