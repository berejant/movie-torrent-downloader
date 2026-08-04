// Package healthcheck signals the outcome of a background job to a
// healthchecks.io-style ping URL.
//
// A job that runs on a timer is invisible when it stops running: nothing in the
// UI says "the last sync was four hours ago". Pinging an external monitor turns
// that silence into an alert, because the monitor is what notices the ping that
// never arrived.
//
// The pinger is optional by construction: New returns nil when no id is
// configured, and every method is a no-op on a nil receiver, so callers never
// branch on whether signalling is switched on.
package healthcheck

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the hosted healthchecks.io ping endpoint. Self-hosted
// installations expose the same paths under their own host.
const DefaultBaseURL = "https://hc-ping.com"

const (
	// pingTimeout bounds one ping. The monitor is not on the critical path of
	// anything, so it never gets to hold up a sync for long.
	pingTimeout = 10 * time.Second

	// A ping is retried because a blip in reaching the monitor would otherwise
	// read as "the job did not run" and raise a false alarm.
	pingAttempts = 3

	// maxBodySize is what healthchecks.io keeps of a ping body and shows as the
	// event log; the rest is dropped, so there is no point sending it.
	maxBodySize = 10 << 10
)

// pingRetryDelay is the pause between attempts. A variable so tests do not pay
// for it.
var pingRetryDelay = 2 * time.Second

// Pinger reports success and failure for one monitored job.
type Pinger struct {
	successURL string
	failURL    string
	http       *http.Client
	logger     *slog.Logger
}

// New builds a pinger for the given check id, or returns nil when the id is
// empty — signalling is off unless it is configured.
func New(baseURL, id string, logger *slog.Logger) *Pinger {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}

	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}

	return &Pinger{
		successURL: base + "/" + id,
		failURL:    base + "/" + id + "/fail",
		http:       &http.Client{Timeout: pingTimeout},
		logger:     logger.With("component", "healthcheck"),
	}
}

// Success reports that the job ran. detail becomes the event log entry on the
// monitor and may be empty.
//
// The nil check is here rather than in ping because the argument — p.successURL
// — is evaluated first, and a nil pinger is the normal "signalling is off" case.
func (p *Pinger) Success(ctx context.Context, detail string) {
	if p == nil {
		return
	}
	p.ping(ctx, p.successURL, "success", detail)
}

// Fail reports that the job is failing, which is what puts the check into the
// down state without waiting for its grace period to expire.
func (p *Pinger) Fail(ctx context.Context, detail string) {
	if p == nil {
		return
	}
	p.ping(ctx, p.failURL, "fail", detail)
}

// Enabled reports whether signalling is configured. It is safe on a nil pinger.
func (p *Pinger) Enabled() bool { return p != nil }

// ping sends one signal, retrying a few times. It never returns an error: the
// caller is a background job that has already done its work and has nothing
// useful to do about an unreachable monitor beyond saying so in the log.
func (p *Pinger) ping(ctx context.Context, target, kind, detail string) {
	if p == nil {
		return
	}

	if len(detail) > maxBodySize {
		detail = detail[:maxBodySize]
	}

	var lastErr error
	for attempt := 1; attempt <= pingAttempts; attempt++ {
		if err := p.send(ctx, target, detail); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return
			}

			if attempt < pingAttempts {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pingRetryDelay):
				}
			}
			continue
		}

		p.logger.Debug("healthcheck signal sent", "kind", kind, "attempt", attempt)
		return
	}

	p.logger.Warn("could not signal the healthcheck monitor",
		"kind", kind, "attempts", pingAttempts, "err", lastErr)
}

func (p *Pinger) send(ctx context.Context, target, detail string) error {
	// The ping carries its own timeout so a caller's long-lived context does
	// not let one attempt hang past pingTimeout.
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(detail))
	if err != nil {
		return fmt.Errorf("healthcheck: build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: ping: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: ping returned status %d", resp.StatusCode)
	}
	return nil
}
