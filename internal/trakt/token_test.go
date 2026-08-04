package trakt

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exampleFile is the shape the Emby/Jellyfin trakt plugin writes.
const exampleFile = `<?xml version="1.0" encoding="utf-8"?>
<PluginConfiguration xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <TraktUsers>
    <TraktUser>
      <AccessToken>Y7NMXugQQIVkb3DV</AccessToken>
      <RefreshToken>HNFkARw1jJs47nKY</RefreshToken>
      <LinkedMbUserId>c38a1b6c-0607-4e4c-8bbf-fc2d50e1f0e1</LinkedMbUserId>
      <ExtraLogging>false</ExtraLogging>
      <AccessTokenExpiration>2026-08-09T22:46:18.308927+03:00</AccessTokenExpiration>
      <DontRemoveItemFromTrakt>true</DontRemoveItemFromTrakt>
    </TraktUser>
  </TraktUsers>
</PluginConfiguration>`

func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "Trakt.xml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestLoadTokenReadsAccessToken(t *testing.T) {
	token, err := LoadToken(writeTokenFile(t, exampleFile))
	if err != nil {
		t.Fatalf("LoadToken() error: %v", err)
	}

	if token.AccessToken != "Y7NMXugQQIVkb3DV" {
		t.Errorf("AccessToken = %q, want Y7NMXugQQIVkb3DV", token.AccessToken)
	}

	want := time.Date(2026, 8, 9, 19, 46, 18, 308927000, time.UTC)
	if !token.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", token.ExpiresAt, want)
	}
	if token.Expired(want.Add(-time.Hour)) {
		t.Error("token reported as expired an hour before its expiry")
	}
	if !token.Expired(want.Add(time.Hour)) {
		t.Error("token not reported as expired an hour after its expiry")
	}
}

// The first usable token wins: the file lists every linked media-server user
// and carries nothing that would identify this service's account.
func TestLoadTokenSkipsUsersWithoutAToken(t *testing.T) {
	const file = `<PluginConfiguration><TraktUsers>
	  <TraktUser><AccessToken></AccessToken></TraktUser>
	  <TraktUser><AccessToken>second</AccessToken></TraktUser>
	</TraktUsers></PluginConfiguration>`

	token, err := LoadToken(writeTokenFile(t, file))
	if err != nil {
		t.Fatalf("LoadToken() error: %v", err)
	}
	if token.AccessToken != "second" {
		t.Errorf("AccessToken = %q, want second", token.AccessToken)
	}
}

// A token without a parseable expiry is still usable: trakt decides.
func TestLoadTokenToleratesMissingExpiry(t *testing.T) {
	const file = `<PluginConfiguration><TraktUsers><TraktUser>
	  <AccessToken>abc</AccessToken><AccessTokenExpiration>not a date</AccessTokenExpiration>
	</TraktUser></TraktUsers></PluginConfiguration>`

	token, err := LoadToken(writeTokenFile(t, file))
	if err != nil {
		t.Fatalf("LoadToken() error: %v", err)
	}
	if token.AccessToken != "abc" {
		t.Errorf("AccessToken = %q, want abc", token.AccessToken)
	}
	if !token.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want the zero time", token.ExpiresAt)
	}
	if token.Expired(time.Now()) {
		t.Error("a token with no recorded expiry must not count as expired")
	}
}

func TestLoadTokenErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "missing file", path: filepath.Join(t.TempDir(), "absent.xml")},
		{name: "no users", path: writeTokenFile(t, `<PluginConfiguration></PluginConfiguration>`)},
		{name: "not xml", path: writeTokenFile(t, `{"access_token":"x"}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadToken(tc.path); err == nil {
				t.Fatal("LoadToken() succeeded, want an error")
			}
		})
	}
}
