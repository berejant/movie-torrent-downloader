package tracker

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	// ErrNoResults means the tracker answered but nothing matched. This is an
	// answer, not a failure: the worker maps it to NOT_FOUND and never retries.
	ErrNoResults = errors.New("tracker: no matching torrents")

	// ErrLoginFailed means the credentials were rejected. Retrying with the
	// same credentials cannot help, so it is not transient.
	ErrLoginFailed = errors.New("tracker: login failed")

	// ErrInvalidTorrent means the downloaded bytes are not a .torrent file,
	// which usually indicates an HTML error page behind a 200 response.
	ErrInvalidTorrent = errors.New("tracker: downloaded content is not a torrent file")
)

// transientError marks a failure that is worth retrying later: network
// trouble, timeouts, 5xx or rate limiting.
type transientError struct{ err error }

func (t transientError) Error() string { return t.err.Error() }
func (t transientError) Unwrap() error { return t.err }

// Transient marks err as retryable.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err: err}
}

// IsTransient reports whether the worker should schedule a retry.
func IsTransient(err error) bool {
	var t transientError
	return errors.As(err, &t)
}

// httpStatusError turns a response status into an error, marking the
// retryable ones as transient.
func httpStatusError(action string, status int) error {
	err := fmt.Errorf("tracker: %s: HTTP %d", action, status)
	if status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout {
		return Transient(err)
	}
	return err
}
