// Package config loads and validates all runtime configuration.
//
// Values come from the process environment. For local development outside
// Docker a .env file in the working directory is loaded first, best effort:
// a missing file is not an error, and real environment variables always win
// over .env entries.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v10"
	"github.com/joho/godotenv"
)

// DefaultUserAgent makes tracker requests look like a current desktop Chrome.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Config is the fully resolved application configuration.
type Config struct {
	HTTPPort        int    `env:"HTTP_PORT" envDefault:"8080"`
	TorrentFilesDir string `env:"TORRENT_FILES_DIR,required"`
	DBPath          string `env:"DB_PATH" envDefault:"/data/app.db"`
	TZ              string `env:"TZ" envDefault:"UTC"`
	LogLevel        string `env:"LOG_LEVEL" envDefault:"info"`
	BatchMaxLines   int    `env:"BATCH_MAX_LINES" envDefault:"100"`

	AuthUser     string `env:"AUTH_USER"`
	AuthPassword string `env:"AUTH_PASSWORD"`

	DuplicateCheckEnabled bool `env:"DUPLICATE_CHECK_ENABLED" envDefault:"true"`

	Tracker Tracker `envPrefix:"TRACKER_"`
	Retry   Retry   `envPrefix:"RETRY_"`
}

// Tracker holds the configuration of a single tracker source. MVP wires up
// exactly one; the struct is shaped so more can be added later.
type Tracker struct {
	Name           string  `env:"NAME" envDefault:"mazepa"`
	BaseURL        string  `env:"BASE_URL,required"`
	Login          string  `env:"LOGIN,required"`
	Password       string  `env:"PASSWORD,required"`
	Priority       int     `env:"PRIORITY" envDefault:"1"`
	TimeoutSeconds int     `env:"TIMEOUT_SECONDS" envDefault:"30"`
	Workers        int     `env:"WORKERS" envDefault:"5"`
	RPS            float64 `env:"RPS" envDefault:"1"`
	MaxSizeBytes   int64   `env:"MAX_SIZE_BYTES" envDefault:"0"`
	UserAgent      string  `env:"USER_AGENT"`

	// ExtraOptions is the raw JSON blob from TRACKER_EXTRA_OPTIONS.
	ExtraOptions string `env:"EXTRA_OPTIONS"`

	// Options is ExtraOptions parsed and merged over the TorrentPier defaults.
	Options TrackerOptions `env:"-"`
}

// Timeout returns the per-request timeout.
func (t Tracker) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

// TrackerOptions are the tracker-specific paths, form fields and selectors.
// Every field is overridable because any tracker theme or engine version can
// differ; the defaults target TorrentPier (mazepa.to).
type TrackerOptions struct {
	TrackerPath string `json:"tracker_path"`
	LoginPath   string `json:"login_path"`

	LoginUsernameField string `json:"login_username_field"`
	LoginPasswordField string `json:"login_password_field"`
	LoginSubmitField   string `json:"login_submit_field"`
	LoginSubmitValue   string `json:"login_submit_value"`

	SearchQueryField string `json:"search_query_field"`

	LoggedInSelector  string `json:"logged_in_selector"`
	LoggedOutSelector string `json:"logged_out_selector"`

	ResultRowSelector    string `json:"result_row_selector"`
	TopicLinkSelector    string `json:"topic_link_selector"`
	DownloadLinkSelector string `json:"download_link_selector"`
	ForumLinkSelector    string `json:"forum_link_selector"`

	// SizeCellIndex is the zero-based <td> holding the size and the download
	// link. TorrentPier renders 0 publish, 1 status, 2 forum, 3 topic,
	// 4 author, 5 size/download, 6 seeders, 7 leechers, 8 replies, 9 added,
	// but column order is the knob most likely to differ between trackers.
	// A negative value skips the direct lookup and scans the whole row.
	SizeCellIndex int `json:"size_cell_index"`
}

// DefaultTrackerOptions returns the TorrentPier defaults.
func DefaultTrackerOptions() TrackerOptions {
	return TrackerOptions{
		TrackerPath:          "/tracker.php",
		LoginPath:            "/login.php",
		LoginUsernameField:   "login_username",
		LoginPasswordField:   "login_password",
		LoginSubmitField:     "login",
		LoginSubmitValue:     "Увійти",
		SearchQueryField:     "nm",
		LoggedInSelector:     "a[href*='logout']",
		LoggedOutSelector:    "#register_link",
		ResultRowSelector:    "#forum_table tbody tr",
		TopicLinkSelector:    "a[href*='topic-']",
		DownloadLinkSelector: "a[href*='dl.php?id=']",
		ForumLinkSelector:    "a[href*='forum-']",
		SizeCellIndex:        5,
	}
}

// Retry describes the persisted retry policy for transient failures.
type Retry struct {
	MaxAttempts       int `env:"MAX_ATTEMPTS" envDefault:"5"`
	BaseSeconds       int `env:"BASE_SECONDS" envDefault:"3"`
	MaxBackoffSeconds int `env:"MAX_BACKOFF_SECONDS" envDefault:"60"`
}

// Load reads .env (if present), binds the environment and validates the result.
func Load() (Config, error) {
	// Best effort: in Docker the values come from the env-file or compose, so
	// an absent .env is the normal case and must not fail startup. Load never
	// overwrites variables that are already set.
	if err := godotenv.Load(); err != nil {
		slog.Debug("no .env file loaded, using process environment", "err", err)
	}

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	options := DefaultTrackerOptions()
	if raw := strings.TrimSpace(cfg.Tracker.ExtraOptions); raw != "" {
		// Unmarshalling over the populated struct keeps defaults for any key
		// the operator did not specify.
		if err := json.Unmarshal([]byte(raw), &options); err != nil {
			return Config{}, fmt.Errorf("config: parse TRACKER_EXTRA_OPTIONS: %w", err)
		}
	}
	cfg.Tracker.Options = options

	if strings.TrimSpace(cfg.Tracker.UserAgent) == "" {
		cfg.Tracker.UserAgent = DefaultUserAgent
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate fails fast with a message naming the offending variable.
func (c Config) Validate() error {
	var problems []string

	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		problems = append(problems, fmt.Sprintf("HTTP_PORT must be 1-65535, got %d", c.HTTPPort))
	}
	if strings.TrimSpace(c.TorrentFilesDir) == "" {
		problems = append(problems, "TORRENT_FILES_DIR must not be empty")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		problems = append(problems, "DB_PATH must not be empty")
	}
	if c.BatchMaxLines < 1 {
		problems = append(problems, fmt.Sprintf("BATCH_MAX_LINES must be >= 1, got %d", c.BatchMaxLines))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel))
	}
	if _, err := time.LoadLocation(c.TZ); err != nil {
		problems = append(problems, fmt.Sprintf("TZ %q is not a known timezone", c.TZ))
	}

	// Basic auth is optional, but half of a credential pair is always a mistake.
	if (c.AuthUser == "") != (c.AuthPassword == "") {
		problems = append(problems, "AUTH_USER and AUTH_PASSWORD must be set together or not at all")
	}

	base, err := url.Parse(strings.TrimRight(c.Tracker.BaseURL, "/"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("TRACKER_BASE_URL is not a valid URL: %v", err))
	case base.Scheme != "http" && base.Scheme != "https":
		problems = append(problems, "TRACKER_BASE_URL must use http or https")
	case base.Host == "":
		problems = append(problems, "TRACKER_BASE_URL must include a host")
	}

	if c.Tracker.Workers < 1 {
		problems = append(problems, fmt.Sprintf("TRACKER_WORKERS must be >= 1, got %d", c.Tracker.Workers))
	}
	if c.Tracker.RPS <= 0 {
		problems = append(problems, fmt.Sprintf("TRACKER_RPS must be > 0, got %v", c.Tracker.RPS))
	}
	if c.Tracker.TimeoutSeconds < 1 {
		problems = append(problems, fmt.Sprintf("TRACKER_TIMEOUT_SECONDS must be >= 1, got %d", c.Tracker.TimeoutSeconds))
	}
	if c.Tracker.MaxSizeBytes < 0 {
		problems = append(problems, "TRACKER_MAX_SIZE_BYTES must be >= 0 (0 means unlimited)")
	}
	if strings.TrimSpace(c.Tracker.Name) == "" {
		problems = append(problems, "TRACKER_NAME must not be empty (it is part of saved filenames)")
	}

	if c.Retry.MaxAttempts < 1 {
		problems = append(problems, fmt.Sprintf("RETRY_MAX_ATTEMPTS must be >= 1, got %d", c.Retry.MaxAttempts))
	}
	if c.Retry.BaseSeconds < 1 {
		problems = append(problems, fmt.Sprintf("RETRY_BASE_SECONDS must be >= 1, got %d", c.Retry.BaseSeconds))
	}
	if c.Retry.MaxBackoffSeconds < c.Retry.BaseSeconds {
		problems = append(problems, "RETRY_MAX_BACKOFF_SECONDS must be >= RETRY_BASE_SECONDS")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// AuthEnabled reports whether the UI and API require HTTP basic auth.
func (c Config) AuthEnabled() bool {
	return c.AuthUser != "" && c.AuthPassword != ""
}

// SlogLevel maps LOG_LEVEL onto slog.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LogValue redacts every secret, so logging the whole config is always safe.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("http_port", c.HTTPPort),
		slog.String("torrent_files_dir", c.TorrentFilesDir),
		slog.String("db_path", c.DBPath),
		slog.String("tz", c.TZ),
		slog.String("log_level", c.LogLevel),
		slog.Int("batch_max_lines", c.BatchMaxLines),
		slog.Bool("auth_enabled", c.AuthEnabled()),
		slog.Bool("duplicate_check_enabled", c.DuplicateCheckEnabled),
		slog.Group("tracker",
			slog.String("name", c.Tracker.Name),
			slog.String("base_url", c.Tracker.BaseURL),
			slog.String("login", redact(c.Tracker.Login)),
			slog.String("password", "[REDACTED]"),
			slog.Int("priority", c.Tracker.Priority),
			slog.Int("workers", c.Tracker.Workers),
			slog.Float64("rps", c.Tracker.RPS),
			slog.Int("timeout_seconds", c.Tracker.TimeoutSeconds),
			slog.Int64("max_size_bytes", c.Tracker.MaxSizeBytes),
		),
		slog.Group("retry",
			slog.Int("max_attempts", c.Retry.MaxAttempts),
			slog.Int("base_seconds", c.Retry.BaseSeconds),
			slog.Int("max_backoff_seconds", c.Retry.MaxBackoffSeconds),
		),
	)
}

// redact keeps just enough of a value to recognise it in logs.
func redact(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 2 {
		return "**"
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1])
}
