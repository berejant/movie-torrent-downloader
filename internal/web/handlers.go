package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/toxa/movie-torrent-downloader/internal/media"
	"github.com/toxa/movie-torrent-downloader/internal/storage"
)

// listLimit caps how many rows the table renders at once.
const listLimit = 500

// tableData is the model behind both the full page and the polled fragment.
type tableData struct {
	Requests []storage.Request
	Counts   map[storage.Status]int
	// Active drives the polling trigger: when nothing is in flight the
	// fragment is rendered without hx-trigger, so an idle page goes quiet.
	Active   bool
	Filter   storage.Status
	Notice   string
	Problem  string
	MaxLines int
	Tracker  string
}

func (s *Server) handleIndex(c echo.Context) error {
	data, err := s.loadTable(c, "", "")
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "index.html", data)
}

func (s *Server) handleTable(c echo.Context) error {
	data, err := s.loadTable(c, "", "")
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "jobs", data)
}

// handleCreateBatch turns the textarea into requests.
func (s *Server) handleCreateBatch(c echo.Context) error {
	raw := c.FormValue("titles")
	force := c.FormValue("force") != ""

	lines, err := s.parseBatch(raw)
	if err != nil {
		return s.renderTable(c, http.StatusUnprocessableEntity, "", err.Error())
	}
	if len(lines) == 0 {
		return s.renderTable(c, http.StatusUnprocessableEntity, "", "Nothing to submit: the list is empty.")
	}

	items := make([]storage.NewRequest, 0, len(lines))
	for _, line := range lines {
		items = append(items, storage.NewRequest{
			RawTitle:        line.raw,
			Query:           line.raw,
			NormalizedQuery: line.normalized,
			Force:           force,
		})
	}

	created, err := s.store.CreateBatch(
		c.Request().Context(),
		s.cfg.Tracker.Name,
		items,
		s.cfg.DuplicateCheckEnabled,
	)
	if err != nil {
		return err
	}

	queued, duplicates := 0, 0
	for _, request := range created {
		if request.Status == storage.StatusDuplicate {
			duplicates++
			continue
		}
		queued++
	}

	if queued > 0 {
		s.notifier.Notify()
	}

	notice := fmt.Sprintf("Queued %d request(s).", queued)
	if duplicates > 0 {
		notice += fmt.Sprintf(" %d rejected as duplicate — edit the query or use Force.", duplicates)
	}
	if skipped := len(lines) - len(created); skipped > 0 {
		notice += fmt.Sprintf(" %d duplicate line(s) collapsed within the batch.", skipped)
	}

	return s.renderTable(c, http.StatusOK, notice, "")
}

// handleRetry re-queues one request, optionally with an edited query. The same
// endpoint serves plain retry, edit-and-retry and force.
func (s *Server) handleRetry(c echo.Context) error {
	id := c.Param("id")
	query := strings.TrimSpace(c.FormValue("query"))
	force := c.FormValue("force") != ""

	normalized := ""
	if query != "" {
		normalized = media.NormalizeQuery(query)
		if normalized == "" {
			return s.renderTable(c, http.StatusUnprocessableEntity, "",
				"The edited query contains no searchable characters.")
		}
	}

	if err := s.store.Retry(c.Request().Context(), id, query, normalized, force); err != nil {
		return s.actionError(c, err)
	}

	s.notifier.Notify()
	return s.renderTable(c, http.StatusOK, "Request re-queued.", "")
}

func (s *Server) handleCancel(c echo.Context) error {
	if err := s.store.Cancel(c.Request().Context(), c.Param("id")); err != nil {
		return s.actionError(c, err)
	}
	return s.renderTable(c, http.StatusOK, "Request cancelled.", "")
}

// handleDelete removes the request and the .torrent file it owned.
func (s *Server) handleDelete(c echo.Context) error {
	path, err := s.store.Delete(c.Request().Context(), c.Param("id"))
	if err != nil {
		return s.actionError(c, err)
	}

	notice := "Request removed."
	switch removed, err := s.removeTorrentFile(path); {
	case err != nil:
		notice += " The .torrent file could not be deleted: " + err.Error()
	case removed:
		notice += " Its .torrent file was deleted."
	}

	return s.renderTable(c, http.StatusOK, notice, "")
}

// handleBatchAction applies one action to every selected row.
func (s *Server) handleBatchAction(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return s.renderTable(c, http.StatusBadRequest, "", "Could not read the form.")
	}

	ids := form["ids"]
	action := c.FormValue("action")
	if len(ids) == 0 {
		return s.renderTable(c, http.StatusUnprocessableEntity, "", "Select at least one request first.")
	}

	ctx := c.Request().Context()
	applied, failed, filesRemoved := 0, 0, 0

	for _, id := range ids {
		var actionErr error

		switch action {
		case "retry":
			actionErr = s.store.Retry(ctx, id, "", "", false)
		case "force":
			actionErr = s.store.Retry(ctx, id, "", "", true)
		case "cancel":
			actionErr = s.store.Cancel(ctx, id)
		case "delete":
			var path string
			path, actionErr = s.store.Delete(ctx, id)
			if actionErr == nil {
				if removed, err := s.removeTorrentFile(path); err == nil && removed {
					filesRemoved++
				}
			}
		default:
			return s.renderTable(c, http.StatusUnprocessableEntity, "", "Unknown batch action.")
		}

		if actionErr != nil {
			// One rejected row must not abort the rest of the selection.
			s.logger.Warn("batch action failed", "action", action, "request_id", id, "err", actionErr)
			failed++
			continue
		}
		applied++
	}

	if applied > 0 && (action == "retry" || action == "force") {
		s.notifier.Notify()
	}

	notice := fmt.Sprintf("%s applied to %d request(s).", titleCase(action), applied)
	if filesRemoved > 0 {
		notice += fmt.Sprintf(" %d .torrent file(s) deleted.", filesRemoved)
	}

	problem := ""
	if failed > 0 {
		problem = fmt.Sprintf("%d request(s) were skipped because their status does not allow %q.", failed, action)
	}

	return s.renderTable(c, http.StatusOK, notice, problem)
}

// titleCase capitalises an ASCII action name for the notice line.
func titleCase(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

// batchLine is one accepted input line.
type batchLine struct {
	raw        string
	normalized string
}

// parseBatch trims, drops blanks, enforces the line limit and collapses
// duplicates within the submission itself.
func (s *Server) parseBatch(raw string) ([]batchLine, error) {
	var (
		lines []batchLine
		seen  = map[string]struct{}{}
		total int
	)

	for _, line := range strings.Split(raw, "\n") {
		title := strings.TrimSpace(line)
		if title == "" {
			continue
		}

		total++
		if total > s.cfg.BatchMaxLines {
			return nil, fmt.Errorf("too many lines: the limit is %d per batch", s.cfg.BatchMaxLines)
		}

		normalized := media.NormalizeQuery(title)
		if normalized == "" {
			continue // punctuation-only line
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}

		lines = append(lines, batchLine{raw: title, normalized: normalized})
	}

	return lines, nil
}

// removeTorrentFile deletes a saved file, refusing any path that resolves
// outside TORRENT_FILES_DIR.
func (s *Server) removeTorrentFile(path string) (bool, error) {
	if path == "" {
		return false, nil
	}

	dir, err := filepath.Abs(s.cfg.TorrentFilesDir)
	if err != nil {
		return false, err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}

	relative, err := filepath.Rel(dir, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false, fmt.Errorf("refusing to delete %s: outside the torrent directory", path)
	}

	if err := os.Remove(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already consumed by a download client: nothing to do.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// actionError turns an expected rejection into a message instead of a 500.
func (s *Server) actionError(c echo.Context, err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return s.renderTable(c, http.StatusNotFound, "", "That request no longer exists.")
	}
	return s.renderTable(c, http.StatusUnprocessableEntity, "", err.Error())
}

// renderTable returns the swapped fragment that every action responds with.
func (s *Server) renderTable(c echo.Context, status int, notice, problem string) error {
	data, err := s.loadTable(c, notice, problem)
	if err != nil {
		return err
	}
	return c.Render(status, "jobs", data)
}

func (s *Server) loadTable(c echo.Context, notice, problem string) (tableData, error) {
	ctx := c.Request().Context()
	filter := storage.Status(strings.ToUpper(strings.TrimSpace(c.QueryParam("status"))))

	requests, err := s.store.List(ctx, filter, listLimit)
	if err != nil {
		return tableData{}, err
	}

	counts, err := s.store.CountsByStatus(ctx)
	if err != nil {
		return tableData{}, err
	}

	active, err := s.store.HasActive(ctx)
	if err != nil {
		return tableData{}, err
	}

	return tableData{
		Requests: requests,
		Counts:   counts,
		Active:   active,
		Filter:   filter,
		Notice:   notice,
		Problem:  problem,
		MaxLines: s.cfg.BatchMaxLines,
		Tracker:  s.cfg.Tracker.Name,
	}, nil
}
