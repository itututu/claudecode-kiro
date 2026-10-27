package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/config"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/server"
	"github.com/d-kuro/kirocc/internal/tokencount"
	"github.com/d-kuro/kirocc/internal/tracing"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := config.ApplyEnvOverrides(&cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logHandler, logCloser := logging.NewHandler(cfg.Debug, cfg.LogFile)
	slog.SetDefault(slog.New(logHandler))
	if cfg.LogFile.Path != "" {
		slog.Info("file logging enabled", "path", cfg.LogFile.Path)
	}

	var otelShutdown func(context.Context) error
	if cfg.OTel {
		shutdown, err := tracing.Init(ctx)
		if err != nil {
			return fmt.Errorf("otel init: %w", err)
		}
		otelShutdown = shutdown
		slog.Info("OpenTelemetry tracing enabled", "body_limit", cfg.OTelBodyLimit)
	}

	authMgr := auth.NewAuthManager(cfg.DBPath, auth.WithAPIKey(cfg.KiroAPIKey, cfg.ServingRegion()))
	if authMgr.UsesAPIKey() {
		// Say so up front: with a key there is no database read and no refresh,
		// so the usual "credentials loaded" line never appears and its absence
		// would otherwise look like a failure.
		slog.Info("using Kiro API key", "auth_type", auth.AuthTypeAPIKey, "db_read", false)
	}
	slog.Info("Kiro serving region", "region", cfg.ServingRegion(), "inference_host", cfg.InferenceHost())
	kiroClient := buildKiroClient(authMgr, cfg)
	srv := buildServer(authMgr, kiroClient, cfg)

	// Eagerly initialize tiktoken so the first API request doesn't block on BPE data fetch.
	go tokencount.Preload()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	slog.Info("kirocc listening", "addr", "http://"+addr)
	slog.Info("set ANTHROPIC_BASE_URL to use with Claude Code", "url", "http://"+addr)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is intentionally not set: this server streams SSE responses
		// that can last minutes. A fixed WriteTimeout would kill long-running streams.
		// Slowloris is mitigated by ReadHeaderTimeout on the request side.
	}

	done := awaitShutdown(httpSrv, otelShutdown, logCloser)

	// Bind before releasing the launcher's startup lock, so the next launcher
	// either waits on the lock or finds a port that is already accepting. There
	// is no window where the lock is free but nothing is listening.
	startupLock, err := inheritedStartupLock(cfg.StartupLockFD)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	if startupLock != nil {
		// Only our copy of the descriptor: the parent keeps its own and decides
		// when the lock itself is released.
		if err := startupLock.Close(); err != nil {
			slog.Warn("closing inherited startup lock", "err", err)
		}
	}

	if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	<-done
	return nil
}

// inheritedStartupLock adopts the startup-lock descriptor the launcher passed
// through, or returns nil when the gateway was started directly without one.
// The descriptor is validated here rather than at close time so a bad number
// fails before the port is taken.
func inheritedStartupLock(fd int) (*os.File, error) {
	if fd == 0 {
		return nil, nil
	}
	f := os.NewFile(uintptr(fd), "startup-lock")
	if f == nil {
		return nil, fmt.Errorf("startup-lock-fd %d: not a valid descriptor", fd)
	}
	if _, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("startup-lock-fd %d: %w", fd, err)
	}
	return f, nil
}

func parseFlags(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("kirocc", flag.ContinueOnError)
	var cfg config.Config
	fs.IntVar(&cfg.Port, "port", 3456, "listen port")
	fs.StringVar(&cfg.Host, "host", "127.0.0.1", "bind host")
	fs.StringVar(&cfg.DBPath, "db", config.DefaultDBPath(), "kiro-cli SQLite DB path")
	fs.StringVar(&cfg.APIKey, "api-key", "", "optional API key for authentication")
	// The upstream Kiro key is deliberately env-only (KIRO_API_KEY): a flag
	// would place the credential in argv, where any process can read it.
	fs.StringVar(&cfg.KiroAPIRegion, "kiro-api-region", "", "Kiro serving region (us-east-1 or eu-central-1); also KIRO_API_REGION")
	fs.BoolVar(&cfg.Debug, "debug", false, "enable debug logging with OTel JSON Lines output")
	fs.BoolVar(&cfg.OTel, "otel", false, "enable OpenTelemetry tracing (OTLP HTTP exporter)")
	fs.IntVar(&cfg.OTelBodyLimit, "otel-body-limit", config.DefaultOTelBodyLimit, "max bytes of request body to capture in OTel spans (0 = unlimited)")
	fs.DurationVar(&cfg.KeepAliveInterval, "keepalive-interval", config.DefaultKeepAliveInterval, "SSE idle keep-alive interval (0 = disabled)")
	fs.StringVar(&cfg.LogFile.Path, "log-file", "", "write logs to file with rotation (for agent debugging)")
	fs.IntVar(&cfg.LogFile.MaxSize, "log-max-size", logging.DefaultLogMaxSize, "max log file size in MB before rotation")
	fs.IntVar(&cfg.LogFile.MaxBackups, "log-max-backups", logging.DefaultLogMaxBackups, "max number of old log files to retain")
	fs.IntVar(&cfg.LogFile.MaxAge, "log-max-age", logging.DefaultLogMaxAge, "max days to retain old log files")
	fs.BoolVar(&cfg.LogFile.Compress, "log-compress", false, "compress rotated log files with gzip")
	fs.BoolVar(&cfg.LogFile.Console, "log-console", false, "also write logs to console when -log-file is set")
	// Non-secret: a descriptor number, not a credential. Flag-only so an
	// exported value can never make us trust a descriptor we did not inherit.
	fs.IntVar(&cfg.StartupLockFD, "startup-lock-fd", 0, "inherited startup-lock descriptor to close once the listener is bound (0 = none)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	// Record which flags the caller actually passed so ApplyEnvOverrides can
	// leave them alone. Without this, an exported default silently wins over
	// an explicit argument.
	fs.Visit(func(f *flag.Flag) { cfg.MarkExplicit(f.Name) })
	return cfg, nil
}

func buildKiroClient(authMgr *auth.AuthManager, cfg config.Config) kiroclient.Client {
	clientOpts := []kiroclient.HTTPClientOption{
		kiroclient.WithTokenCounter(tokencount.CountBytes),
	}
	if authMgr.UsesAPIKey() {
		// API keys are static credentials; rereading one cannot repair a 403.
		clientOpts = append(clientOpts, kiroclient.WithAPIKeyAuth())
	} else {
		clientOpts = append(clientOpts, kiroclient.WithTokenRefresher(func(ctx context.Context) (kiroclient.RefreshedCredentials, error) {
			// Invalidate cache so GetToken re-reads from DB and refreshes
			// instead of returning the same rejected token. The profile ARN
			// travels with the token: a refresh can move both, and the retry is
			// only valid if the request carries the pair that was issued
			// together.
			authMgr.InvalidateCache()
			creds, err := authMgr.GetToken(ctx)
			if err != nil {
				return kiroclient.RefreshedCredentials{}, err
			}
			return kiroclient.RefreshedCredentials{
				AccessToken: creds.AccessToken,
				ProfileARN:  creds.ProfileARN,
			}, nil
		}))
	}
	// Always pin the serving region. Validate has already confirmed it is one
	// Kiro actually serves, so deriving endpoints from it beats inheriting a
	// credential's sign-in region, which is often somewhere Kiro has no
	// inference endpoint at all.
	clientOpts = append(clientOpts, kiroclient.WithRegion(cfg.ServingRegion()))
	if cfg.OTel {
		clientOpts = append(clientOpts, kiroclient.WithOTel(cfg.OTelBodyLimit))
	}
	return kiroclient.NewHTTPClient(clientOpts...)
}

func buildServer(authMgr *auth.AuthManager, client kiroclient.Client, cfg config.Config) *server.Server {
	opts := []server.ServerOption{
		server.WithKeepAliveInterval(cfg.KeepAliveInterval),
		server.WithUpstreamIdentity(authMgr.AuthType(), cfg.ServingRegion(), cfg.InferenceHost()),
	}
	if cfg.OTel {
		opts = append(opts, server.WithOTel(cfg.OTelBodyLimit))
	}
	if cfg.Debug {
		opts = append(opts, server.WithCapture(true))
	}
	return server.New(authMgr, cfg.APIKey, client, opts...)
}

// awaitShutdown registers a SIGINT/SIGTERM handler that gracefully stops the
// HTTP server, flushes OTel spans, and closes the log file. Returns a channel
// that closes when shutdown is complete.
func awaitShutdown(httpSrv *http.Server, otelShutdown func(context.Context) error, logCloser interface{ Close() error }) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		if otelShutdown != nil {
			if err := otelShutdown(ctx); err != nil {
				slog.Error("otel shutdown error", "err", err)
			}
		}
		if err := logCloser.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "log close error: %v\n", err)
		}
		close(done)
	}()
	return done
}
