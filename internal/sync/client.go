package sync

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/audit"
	"github.com/barry/AzureDNSForwarder/internal/config"
)

// ClientConfig wires the peer client loop.
type ClientConfig struct {
	// Provider returns the current config (used to build outgoing envelopes).
	Provider func() *config.File
	// Apply runs the result of a merge: it should take the merged file
	// and persist it via store.Save. It is the only way the client
	// loop mutates the store, and it must validate before writing.
	Apply func(env Envelope, source string) error
	// Audit is used to log pull/push actions.
	Audit *audit.Log
	// Logger for transport errors.
	Logger *slog.Logger
	// HTTPClient is the http.Client used to talk to peers. If nil, a
	// client is built for each request with a TLS config derived from
	// the current PSK. Provide a custom client only in tests or when
	// you need to override transport behaviour.
	HTTPClient *http.Client
	// TLSConfig returns a *tls.Config for the given PSK. Defaults to
	// tlsConfigForClient, which pins the peer certificate to the PSK-
	// derived fingerprint.
	TLSConfig func(psk string) (*tls.Config, error)
	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time
}

// Client is the per-peer pull/push loop driver. It runs one goroutine
// per configured peer and one shared "push on apply" notifier.
type Client struct {
	cfg     ClientConfig
	stopped chan struct{}

	lastMu          sync.Mutex
	// lastAppliedFrom is the InstanceID of the last envelope we
	// successfully applied from a peer. We use this to suppress the
	// echo push that would otherwise fire from the on-save hook and
	// bounce right back at the sender.
	lastAppliedFrom string
	lastAppliedAt   time.Time
}

// NewClient constructs a Client. Call Run to begin.
func NewClient(cfg ClientConfig) *Client {
	if cfg.TLSConfig == nil {
		cfg.TLSConfig = tlsConfigForClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{cfg: cfg, stopped: make(chan struct{})}
}

func (c ClientConfig) httpClient(psk string) (*http.Client, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient, nil
	}
	tlsCfg, err := c.TLSConfig(psk)
	if err != nil {
		return nil, fmt.Errorf("tls config: %w", err)
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, nil
}

// Run starts the per-peer pull loops and the notifier. It returns when
// ctx is cancelled.
func (c *Client) Run(ctx context.Context, peers []config.Peer, settings config.Peers) {
	if c.cfg.PSK() == "" && len(peers) > 0 {
		c.cfg.Logger.Warn("peers configured but no shared key set; sync disabled")
	}
	var wg sync.WaitGroup
	for _, p := range peers {
		if !p.Enabled {
			continue
		}
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			c.runOne(ctx, p, settings)
		}(p)
	}
	<-ctx.Done()
	wg.Wait()
	close(c.stopped)
}

// PSK is a small indirection used internally; the actual PSK lives in
// the settings. The listener and the client both need it.
func (c ClientConfig) PSK() string {
	cur := c.Provider()
	if cur == nil {
		return ""
	}
	return cur.Peers.SharedKey
}

func (c *Client) runOne(ctx context.Context, peer config.Peer, settings config.Peers) {
	interval := time.Duration(settings.SyncIntervalSeconds) * time.Second
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	// Pull once immediately, then on a ticker.
	c.pull(ctx, peer, settings)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pull(ctx, peer, settings)
		}
	}
}

// Push sends a snapshot of the current config to a peer. Called by the
// store's afterSave hook (in main.go) when local changes happen.
// Retries briefly on lock contention so we don't drop the push when
// the peer is busy.
func (c *Client) Push(ctx context.Context, peer config.Peer) error {
	const maxAttempts = 5
	backoff := 100 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.pushOnce(ctx, peer)
		if err == nil {
			return nil
		}
		// Only retry on lock contention; surface other errors immediately.
		if !strings.Contains(err.Error(), "locked") {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

func (c *Client) pushOnce(ctx context.Context, peer config.Peer) error {
	cur := c.cfg.Provider()
	if cur == nil {
		return fmt.Errorf("no config")
	}
	if cur.Peers.SharedKey == "" {
		return fmt.Errorf("no shared key configured")
	}
	if peer.URL == "" {
		return fmt.Errorf("peer %s: empty url", peer.Name)
	}
	env := FilterForExport(instanceID_(), 0, cur, peer.Name)
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	u, err := joinURL(peer.URL, "/peer/v1/items")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Peer-Token", cur.Peers.SharedKey)
	client, err := c.cfg.httpClient(cur.Peers.SharedKey)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var be JSONError
		_ = json.NewDecoder(resp.Body).Decode(&be)
		if be.Error == "" {
			be.Error = resp.Status
		}
		return fmt.Errorf("push to %s: %s", peer.Name, be.Error)
	}
	var mr MergeResult
	if err := json.NewDecoder(resp.Body).Decode(&mr); err == nil {
		if c.cfg.Audit != nil {
			_ = c.cfg.Audit.Write(audit.Entry{
				Actor:   "self",
				Action:  "peer-push-sent",
				Details: peer.Name + " " + StringifyMerge(mr),
			})
		}
	}
	return nil
}

// PushAll pushes the current config to every enabled peer in parallel.
// Errors are returned aggregated.
func (c *Client) PushAll(ctx context.Context, peers []config.Peer) []string {
	// Skip the push if we just applied an envelope from a peer within
	// the last 2 seconds — that means we're the receiver of someone
	// else's push, and pushing back would create a feedback loop.
	c.lastMu.Lock()
	recent := !c.lastAppliedAt.IsZero() && c.cfg.Now().Sub(c.lastAppliedAt) < 2*time.Second
	from := c.lastAppliedFrom
	c.lastAppliedAt = time.Time{}
	c.lastAppliedFrom = ""
	c.lastMu.Unlock()
	if recent {
		if c.cfg.Logger != nil {
			c.cfg.Logger.Info("peer push suppressed (just received from peer)", "from", from)
		}
		return nil
	}
	var errs []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, p := range peers {
		if !p.Enabled {
			continue
		}
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			if err := c.Push(ctx, p); err != nil {
				mu.Lock()
				errs = append(errs, p.Name+": "+err.Error())
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return errs
}

// MarkAppliedFrom records that an envelope was just received from a
// peer. The next PushAll within 2s will be suppressed.
func (c *Client) MarkAppliedFrom(instanceID string) {
	c.lastMu.Lock()
	c.lastAppliedFrom = instanceID
	c.lastAppliedAt = c.cfg.Now()
	c.lastMu.Unlock()
}

func (c *Client) pull(ctx context.Context, peer config.Peer, settings config.Peers) {
	cur := c.cfg.Provider()
	if cur == nil || cur.Peers.SharedKey == "" {
		return
	}
	if peer.URL == "" {
		return
	}
	u, err := joinURL(peer.URL, "/peer/v1/items")
	if err != nil {
		c.recordErr(peer, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		c.recordErr(peer, err)
		return
	}
	req.Header.Set("X-Peer-Token", cur.Peers.SharedKey)
	req.Header.Set("Accept", "application/json")
	client, err := c.cfg.httpClient(cur.Peers.SharedKey)
	if err != nil {
		c.recordErr(peer, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		c.recordErr(peer, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var be JSONError
		_ = json.NewDecoder(resp.Body).Decode(&be)
		if be.Error == "" {
			be.Error = resp.Status
		}
		c.recordErr(peer, fmt.Errorf("pull from %s: %s", peer.Name, be.Error))
		return
	}
	var env Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		c.recordErr(peer, fmt.Errorf("decode: %w", err))
		return
	}
	if c.cfg.Apply == nil {
		c.cfg.Logger.Warn("pull received but Apply is nil", "peer", peer.Name)
		return
	}
	if err := c.cfg.Apply(env, peer.Name); err != nil {
		c.recordErr(peer, err)
		return
	}
	// Suppress the echo push that the on-save hook will fire.
	c.MarkAppliedFrom(env.InstanceID)
	if c.cfg.Audit != nil {
		_ = c.cfg.Audit.Write(audit.Entry{
			Actor:   "peer:" + env.InstanceID,
			Action:  "peer-pull-applied",
			Details: peer.Name,
		})
	}
	c.recordOK(peer)
}

func (c *Client) recordErr(peer config.Peer, err error) {
	// The detailed status lives on the peer; we don't mutate config
	// from here to avoid a save-loop. The HTTP status endpoint reads
	// it via the in-memory map.
	SetPeerStatus(peer.Name, PeerStatusUpdate{
		LastError:  err.Error(),
		LastAction: "error",
		At:         c.cfg.Now(),
	})
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn("peer sync", "peer", peer.Name, "err", err)
	}
}

func (c *Client) recordOK(peer config.Peer) {
	SetPeerStatus(peer.Name, PeerStatusUpdate{
		LastError:  "",
		LastAction: "ok",
		At:         c.cfg.Now(),
	})
}

func joinURL(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = stringsTrimRightSlash(u.Path) + path
	return u.String(), nil
}

func stringsTrimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	if s == "" {
		return ""
	}
	return s + "/"
}
