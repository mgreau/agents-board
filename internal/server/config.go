package server

import (
	"fmt"
	"strings"
)

// Config is everything the binary reads from the environment. See docs/CONTRACT.md
// section (d) for the variable names and defaults.
type Config struct {
	// Addr is the listen address (BOARD_ADDR, default ":8080"; PORT, when set, wins as ":"+PORT).
	Addr string
	// DBPath is the SQLite file (BOARD_DB, default "./board.db").
	DBPath string
	// BaseURL is the public origin without trailing slash, e.g. https://agents-board.mgreau.dev
	// (BOARD_BASE_URL, default "http://localhost:8080"). Used in docs and absolute links.
	BaseURL string
	// AdminToken guards /admin/* (BOARD_ADMIN_TOKEN). Empty disables /admin entirely (404).
	AdminToken string
	// EdgeKey must match X-Board-Edge-Key on every request except /health (BOARD_EDGE_KEY).
	// Empty disables the check.
	EdgeKey string
	// ClientIPHeader names the header carrying the real client IP
	// (BOARD_CLIENT_IP_HEADER, default "X-Envoy-External-Address"; empty = use RemoteAddr).
	ClientIPHeader string
	// SnapshotBucket enables GCS snapshots when non-empty (BOARD_SNAPSHOT_BUCKET).
	SnapshotBucket string
	// SnapshotObject is the object name inside the bucket (BOARD_SNAPSHOT_OBJECT, default "board.db").
	SnapshotObject string
	// OTToken is sent as the Origin-Trial response header when non-empty (BOARD_OT_TOKEN).
	OTToken string
	// Admins is the informational owner handle list shown in docs (BOARD_ADMINS, default ["mgreau"]).
	Admins []string
	// Version is the build version string (set by main from ldflags; "dev" by default).
	Version string
}

// Defaults for Config.
const (
	DefaultAddr           = ":8080"
	DefaultDBPath         = "./board.db"
	DefaultBaseURL        = "http://localhost:8080"
	DefaultClientIPHeader = "X-Envoy-External-Address"
	DefaultSnapshotObject = "board.db"
	DefaultAdmins         = "mgreau"
)

// ConfigFromEnv builds a Config from getenv (os.Getenv in main, a map lookup in tests).
// It applies defaults and validates the little that can be validated without I/O.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	get := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	cfg := Config{
		Addr:           get("BOARD_ADDR", DefaultAddr),
		DBPath:         get("BOARD_DB", DefaultDBPath),
		BaseURL:        strings.TrimRight(get("BOARD_BASE_URL", DefaultBaseURL), "/"),
		AdminToken:     strings.TrimSpace(getenv("BOARD_ADMIN_TOKEN")),
		EdgeKey:        strings.TrimSpace(getenv("BOARD_EDGE_KEY")),
		SnapshotBucket: strings.TrimSpace(getenv("BOARD_SNAPSHOT_BUCKET")),
		SnapshotObject: get("BOARD_SNAPSHOT_OBJECT", DefaultSnapshotObject),
		OTToken:        strings.TrimSpace(getenv("BOARD_OT_TOKEN")),
		Version:        "dev",
	}
	// BOARD_CLIENT_IP_HEADER: unset -> default; set to "" explicitly -> RemoteAddr. Since
	// os.Getenv cannot tell unset from empty, the sentinel value "none" also means RemoteAddr.
	switch v := strings.TrimSpace(getenv("BOARD_CLIENT_IP_HEADER")); {
	case v == "":
		cfg.ClientIPHeader = DefaultClientIPHeader
	case strings.EqualFold(v, "none"):
		cfg.ClientIPHeader = ""
	default:
		cfg.ClientIPHeader = v
	}
	if port := strings.TrimSpace(getenv("PORT")); port != "" {
		cfg.Addr = ":" + port
	}
	for _, a := range strings.Split(get("BOARD_ADMINS", DefaultAdmins), ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.Admins = append(cfg.Admins, a)
		}
	}
	if !strings.HasPrefix(cfg.BaseURL, "http://") && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return Config{}, fmt.Errorf("BOARD_BASE_URL must start with http:// or https://, got %q", cfg.BaseURL)
	}
	return cfg, nil
}

// SnapshotsEnabled reports whether a GCS bucket is configured.
func (c Config) SnapshotsEnabled() bool { return c.SnapshotBucket != "" }
