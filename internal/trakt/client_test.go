package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/berejant/movie-torrent-finder/internal/config"
)

const (
	testClientID = "client-id"
	testToken    = "Y7NMXugQQIVkb3DV"
)

// fakeTrakt serves the movies watchlist from a fixed set of pages, rejecting
// any request that does not carry the four headers trakt requires.
type fakeTrakt struct {
	*httptest.Server

	mu    sync.Mutex
	pages [][]WatchlistItem
	// requested records the page numbers asked for, so a test can prove the
	// cursor stopped the walk instead of only that nothing was queued.
	requested []int
	// status, when non-zero, is returned instead of a page.
	status int
	// omitPageCount drops the pagination header, which is the fallback path
	// where a short page has to end the walk.
	omitPageCount bool
}

func newFakeTrakt(t *testing.T, pages [][]WatchlistItem) *fakeTrakt {
	t.Helper()

	fake := &fakeTrakt{pages: pages}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != watchlistPath {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		for header, want := range map[string]string{
			"Content-Type":      "application/json",
			"trakt-api-version": apiVersion,
			"trakt-api-key":     testClientID,
			"Authorization":     "Bearer " + testToken,
		} {
			if got := r.Header.Get(header); got != want {
				http.Error(w, fmt.Sprintf("header %s = %q, want %q", header, got, want), http.StatusUnauthorized)
				return
			}
		}

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			http.Error(w, "missing page", http.StatusBadRequest)
			return
		}

		fake.mu.Lock()
		fake.requested = append(fake.requested, page)
		status, pages, omit := fake.status, fake.pages, fake.omitPageCount
		fake.mu.Unlock()

		if status != 0 {
			http.Error(w, "boom", status)
			return
		}

		items := []WatchlistItem{}
		if page <= len(pages) {
			items = pages[page-1]
		}

		w.Header().Set("Content-Type", "application/json")
		if !omit {
			w.Header().Set("X-Pagination-Page-Count", strconv.Itoa(len(pages)))
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(fake.Close)

	return fake
}

func (f *fakeTrakt) requestedPages() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.requested...)
}

func (f *fakeTrakt) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(baseURL string) config.Trakt {
	return config.Trakt{
		Enabled:        true,
		ClientID:       testClientID,
		BaseURL:        baseURL,
		TimeoutSeconds: 5,
		PageLimit:      2,
		MaxPages:       5,
		QueryWithYear:  true,
	}
}

func movie(id int64, title string, year int, listedAt string) WatchlistItem {
	when, err := time.Parse(time.RFC3339, listedAt)
	if err != nil {
		panic(err)
	}
	return WatchlistItem{
		ID:       id * 10,
		ListedAt: when,
		Type:     "movie",
		Movie: Movie{
			Title: title,
			Year:  year,
			IDs:   MovieIDs{Trakt: id, Slug: title},
		},
	}
}

func TestWatchlistMoviesParsesTheDocumentedResponse(t *testing.T) {
	// The example from the trakt reference, verbatim.
	const body = `[{"rank":1,"id":1530622086,"listed_at":"2026-08-04T13:38:29.000Z","notes":null,
	  "type":"movie","movie":{"title":"Extraction","year":2020,
	  "ids":{"trakt":396109,"slug":"extraction-2020","imdb":"tt8936646","tmdb":545609}}}]`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/sync/watchlist/movies/listed_at/desc"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		w.Header().Set("X-Pagination-Page-Count", "3")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	client, err := NewClient(testConfig(server.URL), testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	page, err := client.WatchlistMovies(context.Background(), testToken, 1, 100)
	if err != nil {
		t.Fatalf("WatchlistMovies() error: %v", err)
	}

	if page.PageCount != 3 {
		t.Errorf("PageCount = %d, want 3", page.PageCount)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(page.Items))
	}

	item := page.Items[0]
	switch {
	case item.ID != 1530622086:
		t.Errorf("ID = %d, want 1530622086", item.ID)
	case item.Movie.Title != "Extraction":
		t.Errorf("Title = %q, want Extraction", item.Movie.Title)
	case item.Movie.Year != 2020:
		t.Errorf("Year = %d, want 2020", item.Movie.Year)
	case item.Movie.IDs.Trakt != 396109:
		t.Errorf("IDs.Trakt = %d, want 396109", item.Movie.IDs.Trakt)
	case !item.ListedAt.Equal(time.Date(2026, 8, 4, 13, 38, 29, 0, time.UTC)):
		t.Errorf("ListedAt = %v", item.ListedAt)
	}
}

func TestWatchlistMoviesSendsTheRequiredHeaders(t *testing.T) {
	fake := newFakeTrakt(t, [][]WatchlistItem{{movie(1, "Extraction", 2020, "2026-08-04T13:38:29Z")}})

	client, err := NewClient(testConfig(fake.URL), testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	// The fake answers 401 when a required header is missing, so a successful
	// page proves every one of them was sent.
	if _, err := client.WatchlistMovies(context.Background(), testToken, 1, 2); err != nil {
		t.Fatalf("WatchlistMovies() error: %v", err)
	}
}

func TestWatchlistMoviesReportsRejectedCredentials(t *testing.T) {
	fake := newFakeTrakt(t, nil)
	fake.setStatus(http.StatusUnauthorized)

	client, err := NewClient(testConfig(fake.URL), testLogger())
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}

	_, err = client.WatchlistMovies(context.Background(), testToken, 1, 2)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
