package apiserve

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Options configures a Server.
//
// Home and ConfigPath default to the same resolution the picoclaw CLI uses
// (PICOCLAW_HOME / PICOCLAW_CONFIG). Config and Provider are injection seams
// for tests and for embedding the service; when nil they are derived from the
// config file.
type Options struct {
	Home       string
	ConfigPath string
	Debug      bool

	Config   *config.Config
	Provider providers.LLMProvider

	// Settings overrides the runtime knobs on top of DefaultSettings(Home):
	// Host, Port, DBPath, AsyncWorkers, DefaultTimeout and MaxTimeout are each
	// taken from here only when non-zero (see mergeSettings), so a caller can
	// set just the bind address without zeroing the rest. nil means plain
	// DefaultSettings(Home).
	//
	// This seam is what makes the documented internal-network deployment
	// possible: spec §2 promises api.host/api.port with "给内网用改 0.0.0.0"
	// and §4 promises api.async_workers, none of which were reachable while the
	// values were hardcoded in New.
	//
	// Note DBPath is overridable too, but the `picoclaw-api keys` CLI derives
	// its path from ResolveDBPath(Home). Pointing DBPath elsewhere therefore
	// splits the server's database from the CLI's — do it only when a --db flag
	// is passed to both.
	Settings *ServerSettings

	// Listener lets the caller supply an already-bound listener instead of
	// having Start bind Settings.Host:Settings.Port. Tests use it to get an
	// OS-assigned port; production uses it for socket activation or when a
	// supervisor opened a privileged port.
	Listener net.Listener
}

// ServerSettings holds the runtime knobs of the API service.
type ServerSettings struct {
	Host           string
	Port           int
	DBPath         string
	AsyncWorkers   int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

// ResolveHome returns the picoclaw home directory.
func ResolveHome() string { return config.GetHome() }

// ResolveConfigPathFor returns the config.json path inside a specific home,
// honouring PICOCLAW_CONFIG first.
//
// Use this whenever the home is already known — notably from a --home flag.
// ResolveConfigPath resolves the home itself (env var, then the user profile),
// so calling it after --home was parsed silently loads the *default* home's
// config while writing the database into the requested one.
func ResolveConfigPathFor(home string) string {
	if p := os.Getenv(config.EnvConfig); p != "" {
		return p
	}
	return filepath.Join(home, "config.json")
}

// ResolveConfigPath returns the config.json path the CLI would use when no home
// was given explicitly.
func ResolveConfigPath() string {
	return ResolveConfigPathFor(ResolveHome())
}

// ResolveDBPath returns the SQLite path a given home uses. The `picoclaw-api
// keys` CLI relies on this to reach the same file the server opened.
func ResolveDBPath(home string) string {
	return DefaultSettings(home).DBPath
}

// mergeSettings overlays the non-zero fields of override onto base.
//
// A wholesale replacement would be a trap: a caller that only wants to move the
// port would silently zero MaxTimeout (making context.WithTimeout(0, …) expire
// every turn immediately), drop AsyncWorkers to a coerced 1, and — worst — take
// DBPath with it, splitting the server's database from the one the `keys` CLI
// derives from the same home.
func mergeSettings(base, override ServerSettings) ServerSettings {
	if override.Host != "" {
		base.Host = override.Host
	}
	if override.Port != 0 {
		base.Port = override.Port
	}
	if override.DBPath != "" {
		base.DBPath = override.DBPath
	}
	if override.AsyncWorkers != 0 {
		base.AsyncWorkers = override.AsyncWorkers
	}
	if override.DefaultTimeout != 0 {
		base.DefaultTimeout = override.DefaultTimeout
	}
	if override.MaxTimeout != 0 {
		base.MaxTimeout = override.MaxTimeout
	}
	return base
}

// DefaultSettings returns the runtime knobs a given home starts from. Export it
// so the CLI can apply flag overrides (--host/--port) without re-deriving the
// DB path and timeout defaults.
func DefaultSettings(home string) ServerSettings {
	return ServerSettings{
		Host:           "127.0.0.1",
		Port:           18900,
		DBPath:         filepath.Join(home, "apiserve.db"),
		AsyncWorkers:   4,
		DefaultTimeout: 120 * time.Second,
		MaxTimeout:     10 * time.Minute,
	}
}
