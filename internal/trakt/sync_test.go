package trakt

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/toxa/movie-torrent-downloader/internal/config"
	"github.com/toxa/movie-torrent-downloader/internal/storage"
)

// countingNotifier stands in for the worker pool.
type countingNotifier struct{ calls int }

func (n *countingNotifier) Notify() { n.calls++ }

// newSyncer wires a syncer onto a real SQLite store and a fake trakt.
func newSyncer(t *testing.T, fake *fakeTrakt, tweak func(*config.Config)) (*Syncer, *storage.Store, *countingNotifier) {
	t.Helper()

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("storage.Open() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Config{DuplicateCheckEnabled: true, Trakt: testConfig(fake.URL)}
	cfg.Trakt.TokenFile = writeTokenFile(t, exampleFile)
	if tweak != nil {
		tweak(&cfg)
	}

	client, err := NewClient(cfg.Trakt, testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	notifier := &countingNotifier{}
	return NewSyncer(store, client, cfg, notifier, testLogger()), store, notifier
}

func queries(t *testing.T, store *storage.Store) []string {
	t.Helper()

	requests, err := store.List(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	out := make([]string, 0, len(requests))
	for _, request := range requests {
		out = append(out, request.Query)
	}
	return out
}

func TestSyncQueuesWatchlistMovies(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
		movie(1234, "Sicario", 2015, "2026-08-03T10:00:00Z"),
	}})

	syncer, store, notifier := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}

	if summary.Queued != 2 || summary.Scanned != 2 {
		t.Fatalf("summary = %+v, want 2 scanned and 2 queued", summary)
	}
	if notifier.calls != 1 {
		t.Errorf("notifier called %d times, want 1", notifier.calls)
	}

	// Newest first in the response, oldest first in the queue.
	requests, err := store.List(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	for _, request := range requests {
		if request.Status != storage.StatusQueued {
			t.Errorf("%s: status = %s, want QUEUED", request.Query, request.Status)
		}
	}

	got := queries(t, store)
	if !contains(got, "Extraction 2020") || !contains(got, "Sicario 2015") {
		t.Errorf("queries = %v, want the titles with their years", got)
	}
}

func TestSyncQueryWithoutYear(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, func(cfg *config.Config) {
		cfg.Trakt.QueryWithYear = false
	})

	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}

	if got := queries(t, store); len(got) != 1 || got[0] != "Extraction" {
		t.Errorf("queries = %v, want [Extraction]", got)
	}
}

// The point of sorting by listed_at: a second run must not re-queue anything,
// and must not walk past the newest entry it already knows.
func TestSyncSkipsProcessedMoviesAndStopsAtTheCursor(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{
		{
			movie(1, "Newest", 2024, "2026-08-04T13:38:29Z"),
			movie(2, "Middle", 2023, "2026-08-03T13:38:29Z"),
		},
		{
			movie(3, "Older", 2022, "2026-08-02T13:38:29Z"),
			movie(4, "Oldest", 2021, "2026-08-01T13:38:29Z"),
		},
	})

	syncer, store, notifier := newSyncer(t, fake, nil)

	first, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("first SyncOnce() error: %v", err)
	}
	if first.Queued != 4 || first.Pages != 2 {
		t.Fatalf("first summary = %+v, want 4 queued over 2 pages", first)
	}

	second, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	// The entry sitting exactly on the cursor is read again on purpose — a bulk
	// add can give several entries the same listed_at — and dropped by movie id.
	if second.New != 0 || second.Scanned != 1 {
		t.Errorf("second summary = %+v, want 1 scanned and nothing new", second)
	}
	if second.Pages != 1 {
		t.Errorf("second sync fetched %d pages, want 1: the cursor should stop the walk", second.Pages)
	}
	if want := []int{1, 2, 1}; !equalInts(fake.requestedPages(), want) {
		t.Errorf("requested pages = %v, want %v", fake.requestedPages(), want)
	}
	if notifier.calls != 1 {
		t.Errorf("notifier called %d times, want 1: no new work on the second run", notifier.calls)
	}

	if got := len(queries(t, store)); got != 4 {
		t.Errorf("got %d requests, want 4", got)
	}
}

// A movie re-added to the watchlist gets a new entry id and a newer listed_at,
// so the cursor lets it through; the movie id is what stops it.
func TestSyncSkipsAMovieReAddedToTheWatchlist(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, nil)
	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatalf("first SyncOnce() error: %v", err)
	}

	readded := movie(396109, "Extraction", 2020, "2026-08-05T09:00:00Z")
	readded.ID = 999999
	fake.mu.Lock()
	fake.pages = [][]WatchlistItem{{readded}}
	fake.mu.Unlock()

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	if summary.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1: the newer listed_at is past the cursor", summary.Scanned)
	}
	if summary.New != 0 {
		t.Errorf("New = %d, want 0: the movie was already processed", summary.New)
	}
	if got := len(queries(t, store)); got != 1 {
		t.Errorf("got %d requests, want 1", got)
	}
}

// The same entry can arrive twice when the watchlist is edited between two page
// requests. The bookkeeping table is keyed by movie id, so a repeat inside one
// batch would fail the insert if it were not filtered out first.
func TestSyncCollapsesAMovieRepeatedAcrossPages(t *testing.T) {
	repeated := movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z")
	fake := newFakeTrakt(t, [][]WatchlistItem{
		{repeated, movie(2, "Sicario", 2015, "2026-08-03T13:38:29Z")},
		{repeated, movie(3, "Dune", 2021, "2026-08-02T13:38:29Z")},
	})

	syncer, store, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Queued != 3 {
		t.Errorf("Queued = %d, want 3", summary.Queued)
	}
	if got := len(queries(t, store)); got != 3 {
		t.Errorf("got %d requests, want 3", got)
	}
}

// Entries with nothing to search for are counted, not queued.
func TestSyncSkipsEntriesWithoutAMovieID(t *testing.T) {
	broken := movie(0, "No IDs", 2020, "2026-08-04T13:38:29Z")
	broken.Movie.IDs.Trakt = 0

	fake := newFakeTrakt(t, [][]WatchlistItem{{
		broken,
		movie(2, "Sicario", 2015, "2026-08-03T13:38:29Z"),
	}})

	syncer, _, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Skipped != 1 || summary.Queued != 1 {
		t.Errorf("summary = %+v, want 1 skipped and 1 queued", summary)
	}
}

// Without X-Pagination-Page-Count a short page has to end the walk, otherwise
// the syncer would keep asking for empty pages until MaxPages.
func TestSyncStopsOnAShortPageWithoutPaginationHeaders(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	fake.mu.Lock()
	fake.omitPageCount = true
	fake.mu.Unlock()

	syncer, _, _ := newSyncer(t, fake, nil)

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Pages != 1 {
		t.Errorf("Pages = %d, want 1", summary.Pages)
	}
}

// A failure part-way through must queue nothing: a half-applied run would move
// the cursor past entries that were never scheduled.
func TestSyncQueuesNothingWhenAPageFails(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})
	fake.setStatus(http.StatusInternalServerError)

	syncer, store, notifier := newSyncer(t, fake, nil)

	if _, err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("SyncOnce() succeeded, want an error")
	}
	if got := len(queries(t, store)); got != 0 {
		t.Errorf("got %d requests, want 0", got)
	}
	if notifier.calls != 0 {
		t.Errorf("notifier called %d times, want 0", notifier.calls)
	}
}

// A title already downloaded by hand is recorded as processed all the same, so
// the watchlist entry is not reconsidered on every run.
func TestSyncMarksDuplicatesAsProcessed(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{
		movie(396109, "Extraction", 2020, "2026-08-04T13:38:29Z"),
	}})

	syncer, store, _ := newSyncer(t, fake, nil)
	ctx := context.Background()

	created, err := store.CreateBatch(ctx, []storage.NewRequest{{
		RawTitle:        "Extraction 2020",
		Query:           "Extraction 2020",
		NormalizedQuery: "extraction 2020",
	}}, true)
	if err != nil {
		t.Fatalf("CreateBatch() error: %v", err)
	}
	if err := store.MarkDownloaded(ctx, created[0].ID, "/torrents/extraction.torrent"); err != nil {
		t.Fatalf("MarkDownloaded() error: %v", err)
	}

	summary, err := syncer.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("SyncOnce() error: %v", err)
	}
	if summary.Duplicates != 1 || summary.Queued != 0 {
		t.Fatalf("summary = %+v, want 1 duplicate and nothing queued", summary)
	}

	// Second run: the entry is behind the cursor and recorded, so it is gone.
	again, err := syncer.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("second SyncOnce() error: %v", err)
	}
	if again.New != 0 {
		t.Errorf("New = %d on the second run, want 0", again.New)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
