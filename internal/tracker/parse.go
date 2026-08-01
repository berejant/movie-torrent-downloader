package tracker

import (
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/toxa/movie-torrent-downloader/internal/media"
)

// sizeCellIndex is where TorrentPier renders the size and the download link:
// 0 publish, 1 status, 2 forum, 3 topic, 4 author, 5 size/download,
// 6 seeders, 7 leechers, 8 replies, 9 added.
//
// Seeder and leecher columns are parsed by nobody on purpose: results are
// frequently cross-posted with stale swarm numbers, so they are not a signal.
const sizeCellIndex = 5

// parseResults extracts every usable row from a search result page.
func (c *Client) parseResults(doc *goquery.Document, pageURL string) []Torrent {
	base, err := url.Parse(pageURL)
	if err != nil {
		base = c.baseURL
	}

	var torrents []Torrent

	doc.Find(c.options.ResultRowSelector).Each(func(_ int, row *goquery.Selection) {
		torrent, ok := c.parseRow(row, base)
		if !ok {
			return
		}
		if c.cfg.MaxSizeBytes > 0 && torrent.SizeBytes > c.cfg.MaxSizeBytes {
			return
		}
		torrents = append(torrents, torrent)
	})

	return torrents
}

// parseRow reads one result row. A row without both a topic link and a
// download link is not a result (headers, separators, pagination), so it is
// skipped silently.
func (c *Client) parseRow(row *goquery.Selection, base *url.URL) (Torrent, bool) {
	topic := row.Find(c.options.TopicLinkSelector).First()
	download := row.Find(c.options.DownloadLinkSelector).First()
	if topic.Length() == 0 || download.Length() == 0 {
		return Torrent{}, false
	}

	title := strings.TrimSpace(topic.Text())
	downloadHref := strings.TrimSpace(download.AttrOr("href", ""))
	if title == "" || downloadHref == "" {
		return Torrent{}, false
	}

	forum := ""
	if link := row.Find(c.options.ForumLinkSelector).First(); link.Length() > 0 {
		forum = strings.TrimSpace(link.Text())
	}

	sizeText := c.findSizeText(row)

	return Torrent{
		Title:       title,
		Forum:       forum,
		TopicURL:    absoluteURL(base, strings.TrimSpace(topic.AttrOr("href", ""))),
		DownloadURL: absoluteURL(base, downloadHref),
		SizeText:    sizeText,
		Attributes:  media.Analyze(title, sizeText),
	}, true
}

// findSizeText prefers the documented column but falls back to scanning the
// row, so a theme that shifts columns degrades instead of losing every size.
func (c *Client) findSizeText(row *goquery.Selection) string {
	cells := row.Find("td")

	if cells.Length() > sizeCellIndex {
		candidate := strings.TrimSpace(cells.Eq(sizeCellIndex).Text())
		if media.ParseSizeBytes(candidate) > 0 {
			return candidate
		}
	}

	var found string
	cells.EachWithBreak(func(_ int, cell *goquery.Selection) bool {
		text := strings.TrimSpace(cell.Text())
		if media.ParseSizeBytes(text) > 0 {
			found = text
			return false
		}
		return true
	})
	return found
}

func absoluteURL(base *url.URL, href string) string {
	if href == "" {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base == nil {
		return parsed.String()
	}
	return base.ResolveReference(parsed).String()
}
