package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/assets"
	"github.com/barry/AzureDNSForwarder/internal/audit"
	"github.com/barry/AzureDNSForwarder/internal/config"
	"github.com/barry/AzureDNSForwarder/internal/server"
	peersync "github.com/barry/AzureDNSForwarder/internal/sync"
	"github.com/barry/AzureDNSForwarder/internal/unbound/proxy"
	"github.com/barry/AzureDNSForwarder/internal/watcher"
)

func main() {
	var (
		cfgPath    = flag.String("config", envOr("DNSFWD_CONFIG", config.DefaultPath), "Path to the YAML config file")
		outDir     = flag.String("out", envOr("DNSFWD_OUT", "/etc/unbound"), "Directory to write generated unbound.conf")
		confName   = flag.String("conf-name", envOr("DNSFWD_CONF_NAME", "unbound.conf"), "Name of the generated config file inside -out")
		unboundBin = flag.String("unbound-bin", envOr("DNSFWD_UNBOUND_BIN", "unbound"), "unbound binary path")
		instance   = flag.String("instance", envOr("DNSFWD_INSTANCE", defaultInstance()), "Instance identifier for audit log")
		supervise  = flag.Bool("supervise-unbound", true, "Spawn and supervise unbound (recommended)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store := config.NewStore(*cfgPath)
	if err := store.Load(); err != nil {
		logger.Warn("initial config load failed; using defaults", "err", err)
	}

	auditLog := audit.New(filepath.Join(filepath.Dir(*cfgPath), "audit.log"))
	var unboundPID atomic.Int32
	rel := watcher.NewUnboundReloader(&unboundPID, filepath.Join(*outDir, *confName))
	w := watcher.New(store, rel, auditLog, logger, *outDir, *instance, *confName)

	srv, err := server.New(store, w, auditLog, logger, *instance, assets.FS)
	if err != nil {
		logger.Error("server init", "err", err)
		os.Exit(1)
	}

	cur := store.Current()
	listen := "0.0.0.0:80"
	if cur != nil && cur.Settings.HTTPListen != "" {
		listen = cur.Settings.HTTPListen
	}

	httpServer := &http.Server{
		Addr:              listen,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *supervise {
		go superviseUnbound(ctx, logger, *unboundBin, filepath.Join(*outDir, *confName), &unboundPID)
	}

	latencyProxy := proxy.New(store.Current, logger)
	if err := latencyProxy.Start(ctx); err != nil {
		logger.Warn("latency proxy failed to start", "err", err)
	} else {
		w.SetProxy(latencyProxy)
	}

	go w.Run(ctx)

	// ---- Peer sync (optional; only does work if peers.shared_key + peers.list are set) ----
	peersync.SetInstanceID(*instance)
	startPeerListenerAndClient(ctx, logger, store, auditLog, *instance, w)

	go func() {
		logger.Info("listening", "addr", listen, "instance", *instance)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = latencyProxy.Stop()
}

// startPeerListenerAndClient brings up the peer HTTP listener (if a
// shared key is configured) and the per-peer pull/push client loop.
// Push-on-local-save is wired in by replacing the server's afterSave
// handler to also enqueue a push (handled by server.Server's hook).
func startPeerListenerAndClient(ctx context.Context, logger *slog.Logger, store *config.Store, auditLog *audit.Log, instanceID string, w *watcher.Watcher) {
	cur := store.Current()
	if cur == nil {
		return
	}
	if cur.Peers.SharedKey == "" {
		logger.Info("peer sync disabled (no shared key)")
		return
	}
	skew := time.Duration(cur.Peers.ClockSkewSeconds) * time.Second

	// We need a sink for incoming pushes (Listener -> apply to store).
	// The server's afterSave hook is what triggers the unbound reload;
	// we route incoming pushes through store.Save so the same machinery
	// runs.
	pc := peersync.NewClient(peersync.ClientConfig{
		Provider: store.Current,
		Apply: func(env peersync.Envelope, source string) error {
			// Retry on lock contention; the peer is often applying
			// our push at the same moment we are applying theirs.
			const maxAttempts = 20
			backoff := 50 * time.Millisecond
			var err error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				_, err = store.Save(config.SaveOptions{
					Actor: "peer:" + source,
					Mutate: func(f *config.File) error {
						peersync.Merge(f, env, skew)
						return nil
					},
				})
				if err == nil {
					return nil
				}
				if !errors.Is(err, config.ErrLocked) {
					return err
				}
				time.Sleep(backoff)
				backoff *= 2
				if backoff > 500*time.Millisecond {
					backoff = 500 * time.Millisecond
				}
			}
			return err
		},
		Audit:  auditLog,
		Logger: logger,
	})

	// Listener
	ln := peersync.NewListener(peersync.ListenerConfig{
		Listen:   cur.Peers.Listen,
		PSK:      cur.Peers.SharedKey,
		Instance: instanceID,
		Skew:     skew,
		Provider: store.Current,
		PushSink: func(env peersync.Envelope, peerName string) (peersync.MergeResult, error) {
			// Apply via store. Retry briefly on lock contention
			// (the peer is likely applying our previous push at the
			// same moment we are applying theirs).
			before := store.Current()
			if before == nil {
				return peersync.MergeResult{}, fmt.Errorf("no config")
			}
			draft := before.Clone()
			res := peersync.Merge(draft, env, skew)
			// Mark that we just received from this peer so the
			// on-save hook doesn't echo the push right back.
			pc.MarkAppliedFrom(env.InstanceID)
			const maxAttempts = 20
			backoff := 50 * time.Millisecond
			var err error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				_, err = store.Save(config.SaveOptions{
					Actor: "peer:" + env.InstanceID,
					Mutate: func(f *config.File) error {
						peersync.Merge(f, env, skew)
						return nil
					},
				})
				if err == nil {
					return res, nil
				}
				if !errors.Is(err, config.ErrLocked) {
					return res, err
				}
				time.Sleep(backoff)
				backoff *= 2
				if backoff > 500*time.Millisecond {
					backoff = 500 * time.Millisecond
				}
			}
			return res, err
		},
		PeerName: instanceID,
		Audit:    auditLog,
		Logger:   logger,
	})
	if err := ln.Start(); err != nil {
		logger.Warn("peer listener start", "err", err)
		return
	}
	logger.Info("peer listener up", "addr", cur.Peers.Listen)

	// Pull loops
	go pc.Run(ctx, cur.Peers.List, cur.Peers)

	// Push on save: register a hook on the store.
	store.SetOnSave(func() {
		// After a save, the watcher will reload unbound. We also
		// push to all enabled peers. Use a fresh background context
		// with a short timeout so we don't block the save.
		pushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cur := store.Current()
		if cur == nil {
			return
		}
		errs := pc.PushAll(pushCtx, cur.Peers.List)
		for _, e := range errs {
			logger.Warn("peer push", "err", e)
		}
	})

	// Apply hook: ask the watcher to reload unbound right after any
	// save — including peer-driven saves that don't go through the
	// HTTP server's afterSave.
	store.SetOnApply(func() {
		applyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.ApplyNow(applyCtx); err != nil {
			logger.Warn("apply after peer save", "err", err)
		}
	})
}

func superviseUnbound(ctx context.Context, logger *slog.Logger, bin, conf string, pidOut *atomic.Int32) {
	for {
		if _, err := os.Stat(conf); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(ctx, bin, "-d", "-c", conf)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		logger.Info("starting unbound", "bin", bin, "conf", conf)
		if err := cmd.Start(); err != nil {
			logger.Warn("unbound start failed", "err", err)
		} else {
			pidOut.Store(int32(cmd.Process.Pid))
		}
		if err := cmd.Wait(); err != nil {
			logger.Warn("unbound exited", "err", err)
		}
		pidOut.Store(0)
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func defaultInstance() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown"
	}
	return strings.TrimSpace(h)
}

var _ = fmt.Sprintf // keep fmt referenced for future logging helpers
