package storage

import "time"

// Status is the lifecycle state of a single movie request.
type Status string

const (
	// StatusNew is a freshly created request that has not been queued yet.
	StatusNew Status = "NEW"
	// StatusQueued is waiting for a worker, including waiting on a retry backoff.
	StatusQueued Status = "QUEUED"
	// StatusSearching means a worker is querying the tracker.
	StatusSearching Status = "SEARCHING"
	// StatusFound means a topic was selected but the .torrent is not saved yet.
	// It is a transition step, never a resting state.
	StatusFound Status = "FOUND"
	// StatusDownloaded is terminal: the .torrent file is on disk.
	StatusDownloaded Status = "DOWNLOADED"
	// StatusNotFound is terminal: the tracker returned no usable match.
	StatusNotFound Status = "NOT_FOUND"
	// StatusFailed is terminal: retries were exhausted.
	StatusFailed Status = "FAILED"
	// StatusDuplicate is terminal: rejected by the uniqueness check.
	StatusDuplicate Status = "DUPLICATE"
	// StatusCancelled is terminal: cancelled by the operator before it started.
	StatusCancelled Status = "CANCELLED"
)

// Terminal reports whether the status is a resting state that no worker will
// pick up again without an explicit operator action.
func (s Status) Terminal() bool {
	switch s {
	case StatusDownloaded, StatusNotFound, StatusFailed, StatusDuplicate, StatusCancelled:
		return true
	default:
		return false
	}
}

// Cancellable reports whether the request can still be cancelled. In-flight
// work (SEARCHING, FOUND) runs to completion by design.
func (s Status) Cancellable() bool {
	return s == StatusNew || s == StatusQueued
}

// Retryable reports whether the operator may re-queue the request by hand.
func (s Status) Retryable() bool {
	switch s {
	case StatusFailed, StatusNotFound, StatusCancelled, StatusDuplicate:
		return true
	default:
		return false
	}
}

// Request is one movie search request and everything known about its outcome.
type Request struct {
	ID      string
	BatchID string
	// Tracker is the tracker the selected release came from. It is empty until
	// the request reaches FOUND: every tracker is searched, and only the winner
	// is recorded.
	Tracker string

	// RawTitle is the operator's original input line, kept verbatim.
	RawTitle string
	// Query is the effective search string; it can be edited after a NOT_FOUND.
	Query string
	// NormalizedQuery drives duplicate detection.
	NormalizedQuery string

	Status    Status
	LastError string
	// Force bypasses the duplicate check for this request.
	Force bool

	AttemptCount  int
	NextAttemptAt *time.Time

	// Result metadata, populated from FOUND onwards.
	ResultTitle    string
	ResultTopicURL string
	ResultSize     int64
	ResultQuality  string
	ResultCodec    string

	// FilePath is the saved .torrent, set on DOWNLOADED.
	FilePath string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Result carries the tracker metadata of a selected release.
type Result struct {
	// Tracker is the name of the tracker that won the search.
	Tracker  string
	Title    string
	TopicURL string
	Size     int64
	Quality  string
	Codec    string
}

// NewRequest is one line of a batch submission.
type NewRequest struct {
	RawTitle        string
	Query           string
	NormalizedQuery string
	Force           bool
}
