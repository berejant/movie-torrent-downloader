package healthcheck

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

const testUUID = "c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1"

// The retry delay is real time in production and dead time in a test.
func TestMain(m *testing.M) {
	pingRetryDelay = time.Millisecond
	os.Exit(m.Run())
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder collects the pings it receives.
type recorder struct {
	*httptest.Server

	mu    sync.Mutex
	paths []string
	// bodies holds the event log text sent with each ping.
	bodies []string
	// failFirst makes the first n pings answer 500, which is the blip a retry
	// is meant to survive.
	failFirst int
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()

	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		rec.mu.Lock()
		defer rec.mu.Unlock()

		rec.paths = append(rec.paths, r.URL.Path)
		rec.bodies = append(rec.bodies, string(body))

		if rec.failFirst > 0 {
			rec.failFirst--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(rec.Close)

	return rec
}

func (r *recorder) requestedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func TestPingerSignalsSuccessAndFailure(t *testing.T) {
	rec := newRecorder(t)
	pinger := New(rec.URL, testUUID, testLogger())

	pinger.Success(context.Background(), "all good")
	pinger.Fail(context.Background(), "5 consecutive failures")

	want := []string{"/" + testUUID, "/" + testUUID + "/fail"}
	got := rec.requestedPaths()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("paths = %v, want %v", got, want)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.bodies[0] != "all good" || rec.bodies[1] != "5 consecutive failures" {
		t.Errorf("bodies = %v, want the detail text to reach the monitor", rec.bodies)
	}
}

// A blip reaching the monitor must not read as "the job did not run".
func TestPingerRetriesATransientFailure(t *testing.T) {
	rec := newRecorder(t)
	rec.failFirst = 2

	New(rec.URL, testUUID, testLogger()).Success(context.Background(), "")

	if got := len(rec.requestedPaths()); got != 3 {
		t.Fatalf("%d attempts, want 3", got)
	}
}

// Unset id: no pinger, and every method still safe to call.
func TestPingerIsDisabledWithoutAnID(t *testing.T) {
	rec := newRecorder(t)

	pinger := New(rec.URL, "  ", testLogger())
	if pinger != nil {
		t.Fatalf("New() = %v, want nil when no id is configured", pinger)
	}
	if pinger.Enabled() {
		t.Error("Enabled() = true on a nil pinger")
	}

	pinger.Success(context.Background(), "ignored")
	pinger.Fail(context.Background(), "ignored")

	if got := rec.requestedPaths(); len(got) != 0 {
		t.Errorf("sent %v, want no pings at all", got)
	}
}

// An unreachable monitor is logged, never propagated: the job it reports on has
// already done its work.
func TestPingerSurvivesAnUnreachableMonitor(t *testing.T) {
	rec := newRecorder(t)
	url := rec.URL
	rec.Close()

	New(url, testUUID, testLogger()).Success(context.Background(), "")
}

func TestPingerDefaultsToTheHostedEndpoint(t *testing.T) {
	pinger := New("", testUUID, testLogger())
	if want := DefaultBaseURL + "/" + testUUID; pinger.successURL != want {
		t.Errorf("successURL = %q, want %q", pinger.successURL, want)
	}
}
