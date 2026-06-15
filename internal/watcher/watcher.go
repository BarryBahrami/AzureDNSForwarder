package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/audit"
	"github.com/barry/AzureDNSForwarder/internal/config"
	"github.com/barry/AzureDNSForwarder/internal/unbound"
)

type Reloader interface {
	Reload(ctx context.Context) error
}

type unboundReload struct {
	pid *atomic.Int32
}

func NewUnboundReloader(pid *atomic.Int32, confPath string) Reloader {
	return &unboundReload{pid: pid}
}

func (u *unboundReload) Reload(ctx context.Context) error {
	// SIGHUP in unbound 1.20 only reloads forward zones, not local-data.
	// A new local-data record needs a full restart. The supervisor tracks
	// the unbound PID and respawns it on exit, so we just signal the
	// supervised process and let it come back.
	//
	// The supervisor briefly clears the PID to 0 between old-exit and
	// new-start, so we retry a few times with a short wait.
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		pid := int(u.pid.Load())
		if pid > 0 {
			proc, err := os.FindProcess(pid)
			if err != nil {
				lastErr = err
			} else {
				if err := proc.Signal(syscall.SIGTERM); err == nil {
					return nil
				} else {
					lastErr = err
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unbound pid stayed at 0 after retries")
	}
	return fmt.Errorf("signal unbound: %w", lastErr)
}

type Watcher struct {
	store    *config.Store
	rel      Reloader
	audit    *audit.Log
	logger   *slog.Logger
	outDir   string
	confName string
	instance string

	mu        sync.Mutex
	lastError string
	lastOK    time.Time

	cycles atomic.Uint64

	onApplyMu sync.RWMutex
	onApply   func()
	proxy     Proxy
}

// Proxy is the small surface the watcher needs from the latency proxy.
type Proxy interface {
	RefreshNow()
}

func New(store *config.Store, rel Reloader, a *audit.Log, logger *slog.Logger, outDir, instance, confName string) *Watcher {
	return &Watcher{
		store:    store,
		rel:      rel,
		audit:    a,
		logger:   logger,
		outDir:   outDir,
		confName: confName,
		instance: instance,
	}
}

// SetProxy registers the latency proxy so config reloads can refresh its
// zone set. Safe to call with nil to disable.
func (w *Watcher) SetProxy(p Proxy) {
	w.proxy = p
	if p != nil {
		p.RefreshNow()
	}
}

func (w *Watcher) Status() (lastOK time.Time, lastErr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastOK, w.lastError
}

func (w *Watcher) Run(ctx context.Context) {
	cur := w.store.Current()
	if cur == nil {
		w.logger.Warn("no current config; skipping initial apply")
		return
	}
	poll := time.Duration(cur.Settings.PollSeconds) * time.Second
	if poll <= 0 {
		poll = 10 * time.Second
	}
	w.logger.Info("watcher starting", "instance", w.instance, "poll", poll.String())
	if err := w.applyOnce(ctx, "initial"); err != nil {
		w.logger.Error("initial apply failed", "err", err)
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.cycles.Add(1)
			changed, err := w.store.ReloadIfChanged()
			if err != nil {
				w.logger.Warn("reload: parse/validate failed; keeping previous in-memory", "err", err)
				w.setErr(err.Error())
				continue
			}
			if !changed {
				continue
			}
			w.logger.Info("config change detected; regenerating", "instance", w.instance)
			if err := w.applyOnce(ctx, "poll-change"); err != nil {
				w.logger.Error("apply failed; dnsmasq/unbound left untouched", "err", err)
			}
		}
	}
}

func (w *Watcher) setErr(s string) {
	w.mu.Lock()
	w.lastError = s
	w.mu.Unlock()
}

func (w *Watcher) applyOnce(ctx context.Context, reason string) error {
	cur := w.store.Current()
	if cur == nil {
		return fmt.Errorf("no config")
	}

	// Refresh the latency proxy's view of the world before regenerating
	// unbound.conf so the proxy is already listening for any new
	// least-latency zones.
	if w.proxy != nil {
		w.proxy.RefreshNow()
	}

	gen, err := unbound.Generate(cur)
	if err != nil {
		w.setErr(err.Error())
		return err
	}
	if err := os.MkdirAll(w.outDir, 0o755); err != nil {
		w.setErr(err.Error())
		return err
	}
	final := filepath.Join(w.outDir, w.confName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(gen), 0o644); err != nil {
		w.setErr(err.Error())
		return err
	}
	checkCmd := exec.CommandContext(ctx, "unbound-checkconf", tmp)
	checkOut, err := checkCmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(checkOut))
		if msg == "" {
			msg = err.Error()
		}
		w.setErr("unbound-checkconf failed: " + msg)
		os.Remove(tmp)
		return fmt.Errorf("unbound-checkconf failed: %s", msg)
	}
	if err := os.Rename(tmp, final); err != nil {
		w.setErr(err.Error())
		return err
	}
	// On the very first apply (after supervisor started unbound), the file
	// we just wrote is the one unbound loaded. SIGHUP would just race with
	// in-flight queries. Skip it.
	if reason != "initial" {
		if err := w.rel.Reload(ctx); err != nil {
			w.setErr(err.Error())
			return err
		}
	}
	w.mu.Lock()
	w.lastError = ""
	w.lastOK = time.Now()
	w.mu.Unlock()
	_ = w.audit.Write(audit.Entry{
		Actor:   w.instance,
		Action:  "config-applied",
		Details: reason,
	})
	return nil
}

// ApplyNow is used by the HTTP server after a save to reload immediately
// rather than waiting for the next poll cycle.
func (w *Watcher) ApplyNow(ctx context.Context) error {
	return w.applyOnce(ctx, "api-save")
}
