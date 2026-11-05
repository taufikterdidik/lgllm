package config

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Watcher owns the active configuration and swaps it atomically on reload.
//
// Invariant: Current() never returns nil and never returns an invalid config.
// A failed reload leaves the previous config serving, so a bad edit cannot take
// the proxy down.
type Watcher struct {
	path string
	log  *slog.Logger
	// overrides are CLI values re-applied on every load.
	overrides Overrides

	// pollInterval > 0 also watches the file mtime, which helps in containers
	// where sending a signal is awkward.
	pollInterval time.Duration

	// onChange runs before the swap so derived state (the key verifier, the
	// provider index) can be rebuilt and validated. An error rejects the reload
	// and the previous config keeps serving.
	onChange func(*Config) error

	current     atomic.Pointer[Config]
	mu          sync.Mutex
	fingerprint string
}

// NewWatcher loads the initial config and prepares a watcher.
func NewWatcher(path string, ov Overrides, log *slog.Logger, pollInterval time.Duration, onChange func(*Config) error) (*Watcher, error) {
	cfg, err := LoadWithOverrides(path, ov)
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		path:         path,
		log:          log,
		overrides:    ov,
		pollInterval: pollInterval,
		onChange:     onChange,
		fingerprint:  cfg.Fingerprint(),
	}
	w.current.Store(cfg)
	return w, nil
}

// Current returns the live config. Safe for concurrent use.
func (w *Watcher) Current() *Config { return w.current.Load() }

// Reload re-reads the file and reports whether the config actually changed.
func (w *Watcher) Reload() (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := LoadWithOverrides(w.path, w.overrides)
	if err != nil {
		return false, err
	}
	fp := cfg.Fingerprint()
	if fp == w.fingerprint {
		w.log.Debug("config unchanged", "path", w.path, "fingerprint", fp)
		return false, nil
	}

	// Rebuild derived state first: a registry that cannot compile means the new
	// config must not take effect at all.
	if w.onChange != nil {
		if err := w.onChange(cfg); err != nil {
			return false, err
		}
	}

	w.current.Store(cfg)
	w.fingerprint = fp
	w.log.Info("config reloaded", "path", w.path, "fingerprint", fp)
	return true, nil
}

// Run blocks until ctx is cancelled.
//
// It reacts to SIGHUP (signalled through hupCh) and, when pollInterval > 0, to
// file modifications. Failed reloads are logged and the old config keeps
// serving.
func (w *Watcher) Run(ctx context.Context, hupCh <-chan struct{}) {
	var tick <-chan time.Time
	if w.pollInterval > 0 {
		ticker := time.NewTicker(w.pollInterval)
		defer ticker.Stop()
		tick = ticker.C
	}

	var lastMod time.Time
	if fi, err := os.Stat(w.path); err == nil {
		lastMod = fi.ModTime()
	}

	reload := func(trigger string) {
		changed, err := w.Reload()
		switch {
		case err != nil:
			w.log.Error("config reload failed; keeping the previous config",
				"trigger", trigger, "path", w.path, "err", err)
		case changed:
			w.log.Info("config reload applied", "trigger", trigger)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-hupCh:
			if !ok {
				return
			}
			reload("signal")
		case <-tick:
			fi, err := os.Stat(w.path)
			if err != nil {
				w.log.Warn("cannot stat config file", "path", w.path, "err", err)
				continue
			}
			if !fi.ModTime().After(lastMod) {
				continue
			}
			lastMod = fi.ModTime()
			reload("file-change")
		}
	}
}

// SIGHUPSource forwards SIGHUP to a channel consumed by Watcher.Run.
//
// It returns a stop function so the signal handler is released on shutdown.
func SIGHUPSource(ctx context.Context) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	done := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-sigCh:
				select {
				case ch <- struct{}{}:
				default: // a reload is already queued
				}
			}
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(sigCh)
			close(done)
		})
	}
	return ch, stop
}
