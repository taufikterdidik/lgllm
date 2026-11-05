// Command lgllm is a lightweight [OI]-compatible LLM proxy server.
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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"lgllm/internal/auth"
	"lgllm/internal/config"
	"lgllm/internal/registry"
	"lgllm/internal/server"
	"lgllm/internal/upstream"
	"lgllm/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lgllm:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("lgllm", flag.ExitOnError)
	var (
		cfgPath     = fs.String("config", "config.yaml", "path to the YAML config file")
		listen      = fs.String("listen", "", "override server.listen, e.g. 0.0.0.0:20128")
		logLevel    = fs.String("log-level", "", "override log.level: debug|info|warn|error")
		logFormat   = fs.String("log-format", "", "override log.format: text|json")
		configPoll  = fs.Duration("config-poll", 2*time.Second, "poll interval for config changes, 0 disables polling")
		checkOnly   = fs.Bool("check", false, "validate the config and exit")
		showVersion = fs.Bool("version", false, "print build information and exit")
		healthcheck = fs.Bool("healthcheck", false, "probe a running instance and exit 0/1 (for container health checks)")
	)
	fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("lgllm %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return nil
	}
	ov := config.Overrides{Listen: *listen, LogLevel: *logLevel, LogFormat: *logFormat}
	if *healthcheck {
		// A distroless image has no curl, so the binary probes itself.
		return probe(*cfgPath, ov)
	}

	// Validate before the logger exists so config problems print as plain text
	// instead of a structured record the user has to decode. CLI overrides are
	// folded in here and re-applied on every reload by the watcher.
	initial, err := config.LoadWithOverrides(*cfgPath, ov)
	if err != nil {
		return err
	}
	// The registry is compiled once and swapped atomically on reload. Doing it
	// here also means -check validates provider cross-references, not just the
	// YAML syntax.
	reg, err := registry.New(initial)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("config OK: %s (%d provider(s) usable)\n", initial.Source, reg.Len())
		return nil
	}

	logger := newLogger(initial.Log)
	slog.SetDefault(logger)

	var registryPtr atomic.Pointer[registry.Registry]
	registryPtr.Store(reg)

	upstreamClient := upstream.New(initial.Upstream)

	logger.Info("starting lgllm",
		"version", version.String(),
		"config", initial.Source,
		"listen", initial.Server.Listen,
		"auth", initial.Auth.Mode,
		"log_level", initial.Log.Level,
		"providers", reg.Len(),
	)
	if initial.Auth.Mode == "off" {
		logger.Warn("authentication is off; bind to loopback only and never expose this instance")
	}
	if len(initial.Auth.Keys) == 0 && initial.Auth.Mode != "off" {
		logger.Info("no api keys configured; non-loopback clients will be rejected")
	}
	if reg.Len() == 0 {
		logger.Warn("no provider has a usable credential; /v1/chat/completions will answer 404 until connections are added")
	}

	// The verifier lives for the whole process and is updated in place on
	// reload, so a rotated key applies without a restart.
	verifier := auth.New(initial.Auth.Keys, initial.Auth.Mode == "off")

	watcher, err := config.NewWatcher(*cfgPath, ov, logger, *configPoll, func(c *config.Config) error {
		next, err := registry.New(c)
		if err != nil {
			// Reject the whole reload rather than serving with a stale provider
			// index: the file is wrong and the operator needs to fix it.
			return err
		}
		verifier.Replace(c.Auth.Keys, c.Auth.Mode == "off")
		upstreamClient.Reload(c.Upstream)
		registryPtr.Store(next)
		logger.Info("applied new config",
			"auth", c.Auth.Mode,
			"keys", len(c.Auth.Keys),
			"providers", next.Len(),
			"fingerprint", c.Fingerprint(),
		)
		if c.Server.Listen != initial.Server.Listen {
			logger.Warn("server.listen changed in the config, but a restart is required to move the listener",
				"current", initial.Server.Listen, "configured", c.Server.Listen)
		}
		return nil
	})
	if err != nil {
		return err
	}

	srv := server.New(server.Options{
		Config:   watcher.Current,
		Log:      logger,
		Auth:     verifier,
		Registry: registryPtr.Load,
		Upstream: upstreamClient,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hupCh, stopHUP := config.SIGHUPSource(ctx)
	defer stopHUP()
	go watcher.Run(ctx, hupCh)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		// A bind failure or an early serve error is fatal.
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		stop()
		logger.Info("shutdown requested")
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// Release pooled upstream connections so a restart does not leave TIME_WAIT
	// sockets lying around.
	upstreamClient.CloseIdleConnections()

	// HTTP/2 and hijacked connections can linger; do not wait for them forever.
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-time.After(2 * time.Second):
		logger.Warn("server did not exit within the grace period")
	}

	logger.Info("stopped")
	return nil
}

// probe calls /healthz on the locally configured listener and reports failure
// through the exit code, which is what container health checks consume.
func probe(cfgPath string, ov config.Overrides) error {
	cfg, err := config.LoadWithOverrides(cfgPath, ov)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", res.StatusCode)
	}
	fmt.Println("healthy")
	return nil
}

// newLogger builds the structured logger described by cfg.
func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// RFC3339 keeps text logs aligned and parseable in both formats.
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339))
			}
			return a
		},
	}

	var h slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
