// Package media holds the pure functions that decide what a release is and
// which of two releases is better: query normalization, quality and codec
// parsing, size parsing, and the ranking comparator.
//
// Nothing here touches the network or the database, so the selection rules can
// be reasoned about (and tested) in isolation.
package media

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Quality tokens. These are canonical: they are stored in the database and
// embedded in saved filenames, so they must not vary with the release title.
const (
	Quality2160 = "2160p"
	Quality1080 = "1080p"
	Quality720  = "720p"
	QualitySD   = "sd"
)

// Codec tokens.
const (
	CodecH265  = "h265"
	CodecH264  = "h264"
	CodecOther = "other"
)

var (
	// 4K and UHD are label variants of 2160p, not a separate tier.
	re2160 = regexp.MustCompile(`(?i)(\b2160p?\b|\b4k\b|\buhd\b)`)
	re1080 = regexp.MustCompile(`(?i)(\b1080[pi]?\b|\bfull\s*hd\b|\bfhd\b)`)
	re720  = regexp.MustCompile(`(?i)(\b720[pi]?\b|\bhd\s*rip\b|\bhdrip\b|\bhdtv\b|\bhd\b)`)

	reH265 = regexp.MustCompile(`(?i)(\bx\.?265\b|\bh\.?265\b|\bhevc\b)`)
	reH264 = regexp.MustCompile(`(?i)(\bx\.?264\b|\bh\.?264\b|\bavc\b)`)

	// Sizes as TorrentPier renders them, including Ukrainian/Russian units.
	reSize = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?)\s*(B|KB|MB|GB|TB|KIB|MIB|GIB|TIB|Б|КБ|МБ|ГБ|ТБ)`)

	reWhitespace = regexp.MustCompile(`\s+`)
)

// Attributes are the ranking-relevant properties of one release.
type Attributes struct {
	Quality   string
	Codec     string
	SizeBytes int64
}

// Analyze extracts the ranking attributes from a release title and its
// rendered size cell.
func Analyze(title, sizeText string) Attributes {
	return Attributes{
		Quality:   ParseQuality(title),
		Codec:     ParseCodec(title),
		SizeBytes: ParseSizeBytes(sizeText),
	}
}

// ParseQuality maps a release title onto a canonical quality token. Tiers are
// tested from the top down, so "2160p HDR" is 2160p even though "hd" also
// appears in the title.
func ParseQuality(title string) string {
	switch {
	case re2160.MatchString(title):
		return Quality2160
	case re1080.MatchString(title):
		return Quality1080
	case re720.MatchString(title):
		return Quality720
	default:
		return QualitySD
	}
}

// ParseCodec maps a release title onto a canonical codec token.
func ParseCodec(title string) string {
	switch {
	case reH265.MatchString(title):
		return CodecH265
	case reH264.MatchString(title):
		return CodecH264
	default:
		return CodecOther
	}
}

// qualityRank orders the quality tiers; higher is better.
func qualityRank(quality string) int {
	switch quality {
	case Quality2160:
		return 4
	case Quality1080:
		return 3
	case Quality720:
		return 2
	default:
		return 1
	}
}

// codecRank orders the codecs; higher is better. H.265 wins because it carries
// more picture per byte than H.264 at the same file size.
func codecRank(codec string) int {
	switch codec {
	case CodecH265:
		return 3
	case CodecH264:
		return 2
	default:
		return 1
	}
}

// Better reports whether a ranks above b.
//
// Precedence is strict: quality tier, then codec, then tracker priority, then
// larger size. The picture wins over its source — a 2160p release on the
// second-choice tracker beats a 1080p one on the first — and tracker priority
// only separates candidates that are otherwise equally good. Seeder counts are
// deliberately ignored: results are frequently cross-posted between trackers
// with stale or invented swarm numbers.
//
// Callers must use a stable sort so equal candidates keep tracker order.
func Better(a, b Ranked) bool {
	if ra, rb := qualityRank(a.Quality), qualityRank(b.Quality); ra != rb {
		return ra > rb
	}
	if ra, rb := codecRank(a.Codec), codecRank(b.Codec); ra != rb {
		return ra > rb
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority // lower priority value wins
	}
	return a.SizeBytes > b.SizeBytes
}

// Ranked is the minimal view of a candidate needed to order it.
type Ranked struct {
	Attributes
	// Priority is the tracker priority; lower wins. It decides between equally
	// good releases found on different trackers.
	Priority int
}

// NormalizeQuery produces the duplicate-detection key for a search string:
// NFC normalize, lowercase, punctuation to spaces, collapse whitespace, trim.
//
// Punctuation becomes a space rather than being deleted so that "Spider-Man"
// and "Spider Man" collapse to the same key.
func NormalizeQuery(query string) string {
	normalized := norm.NFC.String(query)
	normalized = strings.ToLower(normalized)

	var out strings.Builder
	out.Grow(len(normalized))
	for _, r := range normalized {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
		default:
			// Punctuation, symbols and whitespace all become separators.
			out.WriteRune(' ')
		}
	}

	return strings.TrimSpace(reWhitespace.ReplaceAllString(out.String(), " "))
}

// ParseSizeBytes reads a rendered size such as "1.46 GB" or "700 МБ".
// It returns 0 when nothing parseable is present.
func ParseSizeBytes(value string) int64 {
	// TorrentPier separates the number from the unit with a non-breaking space.
	normalized := strings.ReplaceAll(value, " ", " ")
	match := reSize.FindStringSubmatch(normalized)
	if len(match) != 3 {
		return 0
	}

	number, err := strconv.ParseFloat(strings.Replace(match[1], ",", ".", 1), 64)
	if err != nil {
		return 0
	}

	var multiplier float64 = 1
	switch strings.ToUpper(match[2]) {
	case "KB", "KIB", "КБ":
		multiplier = 1 << 10
	case "MB", "MIB", "МБ":
		multiplier = 1 << 20
	case "GB", "GIB", "ГБ":
		multiplier = 1 << 30
	case "TB", "TIB", "ТБ":
		multiplier = 1 << 40
	}

	return int64(number * multiplier)
}

// HumanSize renders a byte count for the UI.
func HumanSize(bytes int64) string {
	switch {
	case bytes <= 0:
		return ""
	case bytes >= 1<<40:
		return fmt.Sprintf("%.2f TB", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", bytes>>10)
	}
}

// Filename builds the saved .torrent name:
//
//	<title>-<tracker>-<quality>-<requestID>.torrent
//
// The request id makes the name unique per task, so removing one task never
// deletes a file another task still points at.
func Filename(title, tracker, quality, requestID string) string {
	safe := SafeTitle(title)
	if safe == "" {
		safe = "torrent"
	}
	if quality == "" {
		quality = QualitySD
	}
	return fmt.Sprintf("%s-%s-%s-%s.torrent", safe, tracker, quality, requestID)
}

// maxTitleWords keeps the filename readable. Tracker titles carry the whole
// release description — dual titles, year, source, resolution, audio tracks,
// subtitles — and everything past the first few words is already captured by
// the quality token or simply noise:
//
//	Сікаріо 2 / Sicario: Day of the Soldado (2018) UHD BDRemux 4K 2160p HDR 2xUkr/Eng | Sub Eng
//	-> Сікаріо 2 Sicario Day of the Soldado
const maxTitleWords = 7

// SafeTitle turns a release title into the filename fragment.
//
// Separators such as "/", ":" and "|" become spaces rather than underscores:
// an underscore is a visible character that survives into the filename, so
// replacing punctuation with it produced names like "Sicario_ Day _2018_".
func SafeTitle(value string) string {
	var out strings.Builder
	out.Grow(len(value))

	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
		case r == '-', r == '.':
			// Kept so "Spider-Man" and "S.W.A.T." stay intact.
			out.WriteRune(r)
		default:
			out.WriteRune(' ')
		}
	}

	words := strings.Fields(out.String()) // splits and collapses in one pass
	if len(words) > maxTitleWords {
		words = words[:maxTitleWords]
	}

	name := strings.Trim(strings.Join(words, " "), "-. ")

	// Length backstop for a title whose seven words are unusually long.
	const maxLen = 140
	if runes := []rune(name); len(runes) > maxLen {
		name = strings.Trim(strings.TrimSpace(string(runes[:maxLen])), "-. ")
	}
	return name
}
