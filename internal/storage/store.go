// Package storage persists movie requests and their lifecycle in SQLite.
//
// The pool is deliberately limited to a single connection: the workload is
// tiny, and serialising access removes every SQLITE_BUSY race between the five
// workers and the web handlers without any retry plumbing.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // database/sql driver
)

// ErrNotFound is returned when no request matches the given id.
var ErrNotFound = errors.New("storage: request not found")

const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id                TEXT PRIMARY KEY,
    batch_id          TEXT NOT NULL,
    tracker           TEXT NOT NULL,
    raw_title         TEXT NOT NULL,
    query             TEXT NOT NULL,
    normalized_query  TEXT NOT NULL,
    status            TEXT NOT NULL,
    last_error        TEXT NOT NULL DEFAULT '',
    force             INTEGER NOT NULL DEFAULT 0,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   INTEGER,
    result_title      TEXT NOT NULL DEFAULT '',
    result_topic_url  TEXT NOT NULL DEFAULT '',
    result_size       INTEGER NOT NULL DEFAULT 0,
    result_quality    TEXT NOT NULL DEFAULT '',
    result_codec      TEXT NOT NULL DEFAULT '',
    file_path         TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_requests_status     ON requests(status);
CREATE INDEX IF NOT EXISTS idx_requests_normalized ON requests(normalized_query);
CREATE INDEX IF NOT EXISTS idx_requests_created    ON requests(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_requests_claim      ON requests(tracker, status, next_attempt_at);
`

const columns = `id, batch_id, tracker, raw_title, query, normalized_query, status,
	last_error, force, attempt_count, next_attempt_at, result_title, result_topic_url,
	result_size, result_quality, result_codec, file_path, created_at, updated_at`

// Store is the SQLite-backed request repository.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open connects to the SQLite file and applies the schema.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		path,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	// One connection: see the package comment.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: apply schema: %w", err)
	}

	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the database is reachable; used by /health/ready.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// NewID returns a fresh ULID: sortable, compact and filename-safe.
func NewID() string {
	return ulid.Make().String()
}

// CreateBatch inserts one submission. Every line becomes a request, including
// duplicates: a rejected line is stored as DUPLICATE so the operator can edit
// or force it from the list instead of losing it.
//
// checkDuplicates carries DUPLICATE_CHECK_ENABLED; a per-request Force flag
// overrides it for that line.
func (s *Store) CreateBatch(
	ctx context.Context,
	tracker string,
	items []NewRequest,
	checkDuplicates bool,
) ([]Request, error) {
	if len(items) == 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("storage: begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	batchID := NewID()
	now := s.now()
	created := make([]Request, 0, len(items))

	for _, item := range items {
		request := Request{
			ID:              NewID(),
			BatchID:         batchID,
			Tracker:         tracker,
			RawTitle:        item.RawTitle,
			Query:           item.Query,
			NormalizedQuery: item.NormalizedQuery,
			Status:          StatusQueued,
			Force:           item.Force,
			CreatedAt:       now,
			UpdatedAt:       now,
		}

		if checkDuplicates && !item.Force {
			original, err := duplicateOf(ctx, tx, item.NormalizedQuery, "")
			if err != nil {
				return nil, err
			}
			if original != "" {
				request.Status = StatusDuplicate
				request.LastError = "duplicate of request " + original
			}
		}

		if err := insert(ctx, tx, request); err != nil {
			return nil, err
		}
		created = append(created, request)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("storage: commit batch: %w", err)
	}
	return created, nil
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so the duplicate lookup
// works inside a batch transaction and on its own.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// duplicateOf returns the id of an earlier request with the same normalized
// query that reached DOWNLOADED. Only successful downloads count: a title that
// previously failed or was not found may be resubmitted freely.
func duplicateOf(ctx context.Context, db rowQuerier, normalized, excludeID string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx,
		`SELECT id FROM requests
		 WHERE normalized_query = ? AND status = ? AND id <> ?
		 ORDER BY created_at LIMIT 1`,
		normalized, string(StatusDownloaded), excludeID,
	).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("storage: duplicate lookup: %w", err)
	}
	return id, nil
}

func insert(ctx context.Context, tx *sql.Tx, r Request) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO requests (`+columns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.BatchID, r.Tracker, r.RawTitle, r.Query, r.NormalizedQuery,
		string(r.Status), r.LastError, boolToInt(r.Force), r.AttemptCount,
		timeToUnix(r.NextAttemptAt), r.ResultTitle, r.ResultTopicURL, r.ResultSize,
		r.ResultQuality, r.ResultCodec, r.FilePath,
		r.CreatedAt.Unix(), r.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("storage: insert request: %w", err)
	}
	return nil
}

// ClaimNext atomically moves the oldest due QUEUED request to SEARCHING and
// returns it. ok is false when nothing is due.
func (s *Store) ClaimNext(ctx context.Context, tracker string) (Request, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, false, fmt.Errorf("storage: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	row := tx.QueryRowContext(ctx,
		`SELECT `+columns+` FROM requests
		 WHERE tracker = ? AND status = ?
		   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		 ORDER BY created_at
		 LIMIT 1`,
		tracker, string(StatusQueued), now.Unix(),
	)

	request, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE requests SET status = ?, attempt_count = attempt_count + 1, updated_at = ?
		 WHERE id = ?`,
		string(StatusSearching), now.Unix(), request.ID,
	); err != nil {
		return Request{}, false, fmt.Errorf("storage: claim request: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Request{}, false, fmt.Errorf("storage: commit claim: %w", err)
	}

	request.Status = StatusSearching
	request.AttemptCount++
	request.UpdatedAt = now
	return request, true, nil
}

// MarkFound records the selected release before the file is fetched.
func (s *Store) MarkFound(ctx context.Context, id string, result Result) error {
	return s.exec(ctx,
		`UPDATE requests
		 SET status = ?, result_title = ?, result_topic_url = ?, result_size = ?,
		     result_quality = ?, result_codec = ?, last_error = '', updated_at = ?
		 WHERE id = ?`,
		string(StatusFound), result.Title, result.TopicURL, result.Size,
		result.Quality, result.Codec, s.now().Unix(), id,
	)
}

// MarkDownloaded records the saved file and closes the request successfully.
func (s *Store) MarkDownloaded(ctx context.Context, id, filePath string) error {
	return s.exec(ctx,
		`UPDATE requests
		 SET status = ?, file_path = ?, last_error = '', next_attempt_at = NULL, updated_at = ?
		 WHERE id = ?`,
		string(StatusDownloaded), filePath, s.now().Unix(), id,
	)
}

// MarkNotFound closes the request with no usable match. This is an answer, not
// a failure, so it is never retried automatically.
func (s *Store) MarkNotFound(ctx context.Context, id, reason string) error {
	return s.exec(ctx,
		`UPDATE requests SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
		 WHERE id = ?`,
		string(StatusNotFound), reason, s.now().Unix(), id,
	)
}

// MarkFailed closes the request after retries are exhausted.
func (s *Store) MarkFailed(ctx context.Context, id, reason string) error {
	return s.exec(ctx,
		`UPDATE requests SET status = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
		 WHERE id = ?`,
		string(StatusFailed), reason, s.now().Unix(), id,
	)
}

// ScheduleRetry puts the request back in the queue, due at nextAttempt. The
// backoff lives in the row rather than in a worker sleep, so a restart resumes
// the schedule instead of losing it.
func (s *Store) ScheduleRetry(ctx context.Context, id string, nextAttempt time.Time, reason string) error {
	return s.exec(ctx,
		`UPDATE requests SET status = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		string(StatusQueued), nextAttempt.UTC().Unix(), reason, s.now().Unix(), id,
	)
}

// RequeueInFlight resets tasks that were mid-flight when the process stopped.
// Tasks are short and cheap to repeat, so they restart from the beginning.
func (s *Store) RequeueInFlight(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE requests
		 SET status = ?, next_attempt_at = NULL, updated_at = ?
		 WHERE status IN (?, ?)`,
		string(StatusQueued), s.now().Unix(), string(StatusSearching), string(StatusFound),
	)
	if err != nil {
		return 0, fmt.Errorf("storage: requeue in-flight: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(affected), nil
}

// Retry re-queues a terminal request. A non-empty query replaces the search
// string (the NOT_FOUND edit flow); force bypasses the duplicate check.
//
// The duplicate check is re-applied here, not only at submission: retrying a
// DUPLICATE row without forcing it must stay rejected, otherwise the plain
// Retry button would quietly become a Force button.
func (s *Store) Retry(ctx context.Context, id, query, normalized string, force, checkDuplicates bool) error {
	request, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !request.Status.Retryable() {
		return fmt.Errorf("storage: request %s in status %s cannot be retried", id, request.Status)
	}

	if query != "" {
		request.Query = query
		request.NormalizedQuery = normalized
	}
	if force {
		request.Force = true
	}

	status := StatusQueued
	lastError := ""

	if checkDuplicates && !request.Force {
		original, err := duplicateOf(ctx, s.db, request.NormalizedQuery, id)
		if err != nil {
			return err
		}
		if original != "" {
			status = StatusDuplicate
			lastError = "duplicate of request " + original
		}
	}

	return s.exec(ctx,
		`UPDATE requests
		 SET status = ?, query = ?, normalized_query = ?, force = ?,
		     attempt_count = 0, next_attempt_at = NULL, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		string(status), request.Query, request.NormalizedQuery,
		boolToInt(request.Force), lastError, s.now().Unix(), id,
	)
}

// Cancel stops a request that has not started yet.
func (s *Store) Cancel(ctx context.Context, id string) error {
	request, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !request.Status.Cancellable() {
		return fmt.Errorf("storage: request %s in status %s cannot be cancelled", id, request.Status)
	}

	return s.exec(ctx,
		`UPDATE requests SET status = ?, next_attempt_at = NULL, updated_at = ? WHERE id = ?`,
		string(StatusCancelled), s.now().Unix(), id,
	)
}

// Delete removes the request and returns the .torrent path it owned, if any,
// so the caller can unlink it.
func (s *Store) Delete(ctx context.Context, id string) (string, error) {
	request, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if err := s.exec(ctx, `DELETE FROM requests WHERE id = ?`, id); err != nil {
		return "", err
	}
	return request.FilePath, nil
}

// Get loads one request.
func (s *Store) Get(ctx context.Context, id string) (Request, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM requests WHERE id = ?`, id)
	request, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return request, err
}

// List returns requests newest first. A non-empty status filters the result.
func (s *Store) List(ctx context.Context, status Status, limit int) ([]Request, error) {
	if limit <= 0 {
		limit = 500
	}

	query := `SELECT ` + columns + ` FROM requests`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, string(status))
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var requests []Request
	for rows.Next() {
		request, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list requests: %w", err)
	}
	return requests, nil
}

// HasActive reports whether any request is still in a non-terminal state. The
// UI uses it to decide whether the job table should keep polling.
func (s *Store) HasActive(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM requests WHERE status IN (?,?,?,?)`,
		string(StatusNew), string(StatusQueued), string(StatusSearching), string(StatusFound),
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("storage: count active: %w", err)
	}
	return count > 0, nil
}

// CountsByStatus powers the summary line above the job table.
func (s *Store) CountsByStatus(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM requests GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("storage: count by status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[Status]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("storage: count by status: %w", err)
		}
		counts[Status(status)] = count
	}
	return counts, rows.Err()
}

// FilePaths returns every .torrent path the database still owns. Startup uses
// it to tell orphaned temp files from live ones.
func (s *Store) FilePaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_path FROM requests WHERE file_path <> ''`)
	if err != nil {
		return nil, fmt.Errorf("storage: list file paths: %w", err)
	}
	defer func() { _ = rows.Close() }()

	paths := map[string]struct{}{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("storage: list file paths: %w", err)
		}
		paths[filepath.Clean(path)] = struct{}{}
	}
	return paths, rows.Err()
}

// Backoff returns the delay before attempt number attempt (1-based),
// exponential from base and capped at max, with full jitter so a batch of
// failures does not retry in lockstep.
func Backoff(attempt, baseSeconds, maxSeconds int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	delay := float64(baseSeconds)
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= float64(maxSeconds) {
			delay = float64(maxSeconds)
			break
		}
	}

	// Full jitter over the lower half keeps the delay meaningful but spread.
	jitter := 0.5 + rand.Float64()/2 //nolint:gosec // scheduling jitter, not crypto
	return time.Duration(delay * jitter * float64(time.Second))
}

func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(src scanner) (Request, error) {
	var (
		r         Request
		status    string
		force     int
		nextAt    sql.NullInt64
		createdAt int64
		updatedAt int64
	)

	err := src.Scan(
		&r.ID, &r.BatchID, &r.Tracker, &r.RawTitle, &r.Query, &r.NormalizedQuery,
		&status, &r.LastError, &force, &r.AttemptCount, &nextAt,
		&r.ResultTitle, &r.ResultTopicURL, &r.ResultSize, &r.ResultQuality,
		&r.ResultCodec, &r.FilePath, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Request{}, err
		}
		return Request{}, fmt.Errorf("storage: scan request: %w", err)
	}

	r.Status = Status(status)
	r.Force = force != 0
	r.CreatedAt = time.Unix(createdAt, 0).UTC()
	r.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if nextAt.Valid {
		next := time.Unix(nextAt.Int64, 0).UTC()
		r.NextAttemptAt = &next
	}

	return r, nil
}

func timeToUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// TrimError shortens an error for storage in last_error.
func TrimError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	const maxLen = 500
	if len(message) > maxLen {
		return message[:maxLen] + "…"
	}
	return message
}
