package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/d-kuro/kirocc/internal/logging"
)

const (
	// DefaultOTelBodyLimit is the default max bytes of request body to capture in OTel spans.
	DefaultOTelBodyLimit = 32 * 1024
	// DefaultKeepAliveInterval is the default idle time between SSE keep-alive comments.
	DefaultKeepAliveInterval = 15 * time.Second
	// DefaultServingRegion is the region used when none is configured. Kiro
	// exposes inference in a small, fixed set of regions; managed commercial
	// operation defaults here rather than guessing from a credential.
	DefaultServingRegion = "us-east-1"
)

// servingRegions is the exhaustive set of regions where Kiro exposes an
// inference endpoint. Anything else is rejected before bind: probing or
// failing over to an unsupported region would produce an opaque upstream
// rejection rather than a configuration error.
var servingRegions = map[string]struct{}{
	"us-east-1":    {},
	"eu-central-1": {},
}

// Config is the runtime configuration for kirocc.
type Config struct {
	Port   int
	Host   string
	DBPath string
	APIKey string // guards kirocc's own endpoints; unrelated to KiroAPIKey
	// KiroAPIKey is a Kiro API key ("ksk_…") used upstream instead of the
	// kiro-cli database credential. Named after Kiro's own KIRO_API_KEY rather
	// than the KIROCC_* convention, since it is Kiro's credential, not kirocc's.
	KiroAPIKey string
	// KiroAPIRegion pins Kiro's runtime/MCP endpoint region without changing
	// the credential's issuer region used for token refresh.
	KiroAPIRegion     string
	Debug             bool
	OTel              bool
	OTelBodyLimit     int
	KeepAliveInterval time.Duration
	LogFile           logging.LogFileConfig
	// StartupLockFD is an inherited file descriptor holding the launcher's
	// startup lock. The gateway closes its copy only once the listener is bound,
	// so a concurrent launcher cannot observe the lock as free before the port
	// is actually accepting. Zero means no descriptor was passed, which is the
	// correct answer for an unmanaged direct startup.
	//
	// Flag-only on purpose: it has no environment counterpart, so an exported
	// value can never make the gateway trust a descriptor it did not inherit.
	StartupLockFD int

	// explicit holds the names of flags the caller actually passed on the
	// command line. An explicit flag outranks its environment counterpart, so
	// a launcher may export defaults without overriding a deliberate argument.
	// Nil means "nothing was explicit", which is the correct answer for a
	// config built as a struct literal.
	explicit map[string]struct{}
}

const maxRegionLen = 64

// MarkExplicit records that flagName was passed on the command line.
func (c *Config) MarkExplicit(flagName string) {
	if c.explicit == nil {
		c.explicit = make(map[string]struct{})
	}
	c.explicit[flagName] = struct{}{}
}

// IsExplicit reports whether flagName was passed on the command line.
func (c *Config) IsExplicit(flagName string) bool {
	_, ok := c.explicit[flagName]
	return ok
}

// ServingRegion is the single region used to derive every Kiro endpoint.
// Validate guarantees the value is supported, so this never guesses.
func (c *Config) ServingRegion() string {
	if c.KiroAPIRegion == "" {
		return DefaultServingRegion
	}
	return c.KiroAPIRegion
}

// InferenceHost is the Kiro runtime host for the serving region.
func (c *Config) InferenceHost() string {
	return "runtime." + c.ServingRegion() + ".kiro.dev"
}

// InferenceEndpoint is the Kiro runtime URL for the serving region.
func (c *Config) InferenceEndpoint() string {
	return "https://" + c.InferenceHost() + "/"
}

// WebSearchEndpoint is the native WebSearch MCP URL for the serving region.
func (c *Config) WebSearchEndpoint() string {
	return "https://q." + c.ServingRegion() + ".amazonaws.com/mcp"
}

// DefaultDBPath returns the default kiro-cli SQLite database location.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return DefaultDBPathFor(runtime.GOOS, home)
}

// DefaultDBPathFor returns the default database location for the given OS and home directory.
func DefaultDBPathFor(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	case "windows":
		// Kiro CLI 2.x keeps its SQLite state under the same user-local
		// hierarchy on native Windows. The PowerShell launcher also probes
		// LOCALAPPDATA and APPDATA and sets KIROCC_DB_PATH when needed.
		return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
	default:
		return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
	}
}

// ApplyEnvOverrides mutates cfg using KIROCC_* environment variables. An
// environment variable is ignored when the flag it mirrors was passed
// explicitly: the command line is the more deliberate of the two inputs.
func ApplyEnvOverrides(cfg *Config) error {
	cfg.envString("KIROCC_DB_PATH", "db", &cfg.DBPath)
	cfg.envString("KIROCC_API_KEY", "api-key", &cfg.APIKey)
	// Kiro's own variable names, so a machine already set up for headless
	// kiro-cli needs no kirocc-specific configuration. The upstream key has no
	// flag to outrank it: it is environment-only, so it never reaches argv.
	applyString("KIRO_API_KEY", &cfg.KiroAPIKey)
	cfg.envString("KIRO_API_REGION", "kiro-api-region", &cfg.KiroAPIRegion)
	cfg.envString("KIROCC_HOST", "host", &cfg.Host)
	if err := cfg.envInt("KIROCC_PORT", "port", &cfg.Port); err != nil {
		return err
	}
	if err := cfg.envBool("KIROCC_DEBUG", "debug", &cfg.Debug); err != nil {
		return err
	}
	if err := cfg.envBool("KIROCC_OTEL", "otel", &cfg.OTel); err != nil {
		return err
	}
	if err := cfg.envInt("KIROCC_OTEL_BODY_LIMIT", "otel-body-limit", &cfg.OTelBodyLimit); err != nil {
		return err
	}
	if err := cfg.envDuration("KIROCC_KEEPALIVE_INTERVAL", "keepalive-interval", &cfg.KeepAliveInterval); err != nil {
		return err
	}
	cfg.envString("KIROCC_LOG_FILE", "log-file", &cfg.LogFile.Path)
	if err := cfg.envInt("KIROCC_LOG_MAX_SIZE", "log-max-size", &cfg.LogFile.MaxSize); err != nil {
		return err
	}
	if err := cfg.envInt("KIROCC_LOG_MAX_BACKUPS", "log-max-backups", &cfg.LogFile.MaxBackups); err != nil {
		return err
	}
	if err := cfg.envInt("KIROCC_LOG_MAX_AGE", "log-max-age", &cfg.LogFile.MaxAge); err != nil {
		return err
	}
	if err := cfg.envBool("KIROCC_LOG_COMPRESS", "log-compress", &cfg.LogFile.Compress); err != nil {
		return err
	}
	if err := cfg.envBool("KIROCC_LOG_CONSOLE", "log-console", &cfg.LogFile.Console); err != nil {
		return err
	}
	return nil
}

func (c *Config) envString(key, flagName string, dst *string) {
	if c.IsExplicit(flagName) {
		return
	}
	applyString(key, dst)
}

func (c *Config) envInt(key, flagName string, dst *int) error {
	if c.IsExplicit(flagName) {
		return nil
	}
	return applyInt(key, dst)
}

func (c *Config) envBool(key, flagName string, dst *bool) error {
	if c.IsExplicit(flagName) {
		return nil
	}
	return applyBool(key, dst)
}

func (c *Config) envDuration(key, flagName string, dst *time.Duration) error {
	if c.IsExplicit(flagName) {
		return nil
	}
	return applyDuration(key, dst)
}

// Validate checks that the config is internally consistent. Returns an error
// describing the first violation found. Called after flag parsing and env
// overrides, before the server starts.
func (c *Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	// The gateway holds a credential and answers with a fixed local bearer
	// only. Binding anywhere but loopback would expose both to the network.
	if !isLoopbackHost(c.Host) {
		return fmt.Errorf("host must be a loopback address (127.0.0.1, localhost, or ::1), got %q", c.Host)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}
	if c.OTelBodyLimit < 0 {
		return fmt.Errorf("otel-body-limit must be >= 0, got %d", c.OTelBodyLimit)
	}
	if c.KeepAliveInterval != 0 && c.KeepAliveInterval < time.Second {
		return fmt.Errorf("keepalive-interval must be 0 or >= 1s, got %s", c.KeepAliveInterval)
	}
	if c.KiroAPIRegion != "" && !isServingRegion(c.KiroAPIRegion) {
		return fmt.Errorf("kiro-api-region must be one of us-east-1, eu-central-1, got %q", c.KiroAPIRegion)
	}
	// 0, 1 and 2 are this process's own stdio. Accepting one of them would make
	// the gateway close its own standard stream after bind.
	if c.StartupLockFD != 0 && c.StartupLockFD <= 2 {
		return fmt.Errorf("startup-lock-fd must be > 2, got %d", c.StartupLockFD)
	}
	return nil
}

func isServingRegion(region string) bool {
	if len(region) > maxRegionLen {
		return false
	}
	_, ok := servingRegions[region]
	return ok
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func applyString(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func applyInt(key string, dst *int) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = n
	}
	return nil
}

func applyBool(key string, dst *bool) error {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = b
	}
	return nil
}

func applyDuration(key string, dst *time.Duration) error {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = d
	}
	return nil
}
