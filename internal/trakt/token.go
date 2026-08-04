package trakt

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"
)

// maxTokenFileSize is a sanity bound on Trakt.xml: the real file is a couple of
// kilobytes, and reading a wrong path should fail rather than eat memory.
const maxTokenFileSize = 1 << 20

// Token is what this service needs out of Trakt.xml.
type Token struct {
	AccessToken string
	// ExpiresAt is the expiry the owning application recorded, zero when the
	// file does not carry one. It is only used to warn: the token is sent as
	// found, and trakt is the authority on whether it still works.
	ExpiresAt time.Time
}

// Expired reports whether the recorded expiry is in the past.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && t.ExpiresAt.Before(now)
}

// pluginConfiguration mirrors the parts of Trakt.xml this service reads. The
// file is written by the Emby/Jellyfin trakt plugin, which owns the OAuth
// refresh; everything else in it is ignored.
type pluginConfiguration struct {
	Users []traktUser `xml:"TraktUsers>TraktUser"`
}

type traktUser struct {
	AccessToken string `xml:"AccessToken"`
	Expiration  string `xml:"AccessTokenExpiration"`
}

// LoadToken reads the access token from Trakt.xml.
//
// It is called before every sync rather than cached at startup, because the
// application that owns the file refreshes the token in place and this service
// must pick that up without a restart.
func LoadToken(path string) (Token, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Token{}, fmt.Errorf("trakt: token file %s: %w", path, err)
	}
	if info.Size() > maxTokenFileSize {
		return Token{}, fmt.Errorf("trakt: token file %s is %d bytes, which is not a Trakt.xml", path, info.Size())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Token{}, fmt.Errorf("trakt: read token file %s: %w", path, err)
	}

	var parsed pluginConfiguration
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return Token{}, fmt.Errorf("trakt: parse token file %s: %w", path, err)
	}

	// The file holds a list because the plugin can link several media-server
	// users. Only one of them is this service's account, and there is nothing in
	// the file to tell them apart, so the first usable token wins.
	for _, user := range parsed.Users {
		token := strings.TrimSpace(user.AccessToken)
		if token == "" {
			continue
		}

		expiry, err := parseExpiration(user.Expiration)
		if err != nil {
			// A malformed date is not worth refusing a working token over.
			expiry = time.Time{}
		}
		return Token{AccessToken: token, ExpiresAt: expiry}, nil
	}

	return Token{}, fmt.Errorf("trakt: token file %s contains no access token", path)
}

// parseExpiration accepts the timestamp shapes .NET's XML serializer writes:
// an offset ("2026-08-09T22:46:18.308927+03:00"), a UTC "Z", or no zone at all,
// which is read as UTC.
func parseExpiration(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("trakt: unrecognised timestamp %q", value)
}
