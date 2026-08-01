package tracker

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// requestKind selects the header set. Trackers fingerprint clients on more
// than the User-Agent, so a plain Go request stands out immediately.
type requestKind int

const (
	// kindDocument is a normal page navigation.
	kindDocument requestKind = iota
	// kindForm is a form submission (the login POST).
	kindForm
	// kindDownload fetches the .torrent file itself.
	kindDownload
)

const (
	acceptDocument = "text/html,application/xhtml+xml,application/xml;q=0.9," +
		"image/avif,image/webp,image/apng,*/*;q=0.8"
	acceptTorrent  = "application/x-bittorrent,*/*;q=0.8"
	acceptLanguage = "uk-UA,uk;q=0.9,ru;q=0.8,en-US;q=0.7,en;q=0.6"
)

// do applies the shared rate limit, sets browser-like headers and executes the
// request. Every tracker request in the package goes through here.
func (c *Client) do(
	ctx context.Context,
	method, target string,
	body io.Reader,
	kind requestKind,
	referer string,
) (*http.Response, error) {
	// All workers share this limiter, so concurrency never turns into a burst.
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, Transient(fmt.Errorf("tracker: rate limiter: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("tracker: build request: %w", err)
	}

	c.setHeaders(req, kind, referer)

	resp, err := c.http.Do(req)
	if err != nil {
		// Connection failures and timeouts are worth another attempt later.
		return nil, Transient(fmt.Errorf("tracker: %s %s: %w", method, target, err))
	}
	return resp, nil
}

func (c *Client) setHeaders(req *http.Request, kind requestKind, referer string) {
	header := req.Header

	header.Set("User-Agent", c.cfg.UserAgent)
	header.Set("Accept-Language", acceptLanguage)
	header.Set("Connection", "keep-alive")

	// Accept-Encoding is deliberately unset: Go's transport adds gzip and
	// transparently decompresses only while it owns that header.

	if referer != "" {
		header.Set("Referer", referer)
	}

	switch kind {
	case kindDownload:
		header.Set("Accept", acceptTorrent)
		header.Set("Sec-Fetch-Dest", "empty")
		header.Set("Sec-Fetch-Mode", "no-cors")
		header.Set("Sec-Fetch-Site", "same-origin")

	case kindForm:
		header.Set("Content-Type", "application/x-www-form-urlencoded")
		header.Set("Accept", acceptDocument)
		header.Set("Origin", c.baseURL.Scheme+"://"+c.baseURL.Host)
		header.Set("Upgrade-Insecure-Requests", "1")
		header.Set("Sec-Fetch-Dest", "document")
		header.Set("Sec-Fetch-Mode", "navigate")
		header.Set("Sec-Fetch-Site", "same-origin")
		header.Set("Sec-Fetch-User", "?1")

	default:
		header.Set("Accept", acceptDocument)
		header.Set("Upgrade-Insecure-Requests", "1")
		header.Set("Sec-Fetch-Dest", "document")
		header.Set("Sec-Fetch-Mode", "navigate")
		if referer == "" {
			header.Set("Sec-Fetch-Site", "none")
		} else {
			header.Set("Sec-Fetch-Site", "same-origin")
		}
	}
}
