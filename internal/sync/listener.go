package sync

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/audit"
	"github.com/barry/AzureDNSForwarder/internal/config"
)

// ListenerConfig wires the peer HTTP server. All fields are required.
type ListenerConfig struct {
	Listen   string        // e.g. "0.0.0.0:8443" (inside the WG netns in prod)
	PSK      string        // shared key; if empty the listener refuses all
	Instance string        // our instance id, returned in Manifest/Envelope
	Skew     time.Duration // accepted envelope timestamp tolerance
	// PeerName is the name this listener uses to identify inbound peers.
	// It is used when filtering the exported config so a peer only sees
	// items meant for it. Defaults to "default" if empty.
	PeerName string
	// Provider gives the listener a snapshot of the current config on
	// every request. The listener does NOT mutate the store; pushes
	// are applied by the client loop after the store validates them.
	Provider func() *config.File
	// PushSink accepts an envelope from a remote peer and the name of the
	// peer that sent it. It returns the MergeResult and an error. The
	// listener writes the response.
	PushSink func(env Envelope, peerName string) (MergeResult, error)
	// Audit is used to log who connected.
	Audit *audit.Log
	// Logger for transport errors.
	Logger *slog.Logger
}

func (cfg ListenerConfig) peerName() string {
	if cfg.PeerName != "" {
		return cfg.PeerName
	}
	return "default"
}

// Listener is the peer HTTP server.
type Listener struct {
	cfg ListenerConfig
	srv *http.Server
}

// NewListener builds a Listener. Use Start to begin serving HTTPS using
// a TLS certificate derived from the configured PSK.
func NewListener(cfg ListenerConfig) *Listener {
	mux := http.NewServeMux()
	l := &Listener{cfg: cfg}
	mux.HandleFunc("/peer/v1/manifest", l.handleManifest)
	mux.HandleFunc("/peer/v1/items", l.handleItems)
	mux.HandleFunc("/peer/v1/healthz", l.handleHealthz)
	l.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return l
}

// StartTLS starts the listener with the supplied tls.Config. Use it only
// when you need to inject a custom certificate; otherwise call Start.
func (l *Listener) StartTLS(tlsCfg *tls.Config) error {
	if l.cfg.PSK == "" {
		return fmt.Errorf("refusing to start peer listener without a shared key (set peers.shared_key or PEER_SHARED_KEY)")
	}
	l.srv.TLSConfig = tlsCfg
	ln, err := net.Listen("tcp", l.cfg.Listen)
	if err != nil {
		return err
	}
	go func() {
		err := l.srv.ServeTLS(ln, "", "")
		if err != nil && err != http.ErrServerClosed {
			l.cfg.Logger.Error("peer listener", "err", err)
		}
	}()
	return nil
}

// Start starts the listener with a TLS certificate derived from the
// preshared key. All peer sync traffic is encrypted and the certificate
// is pinned by clients through the same PSK-derived fingerprint.
func (l *Listener) Start() error {
	tlsCfg, err := tlsConfigForServer(l.cfg.PSK)
	if err != nil {
		return fmt.Errorf("derive peer TLS cert: %w", err)
	}
	return l.StartTLS(tlsCfg)
}

// Stop gracefully shuts the listener down.
func (l *Listener) Stop(ctx context.Context) error {
	if l.srv == nil {
		return nil
	}
	return l.srv.Shutdown(ctx)
}

// Addr returns the bound address (useful for tests that pass :0).
func (l *Listener) Addr() string { return l.cfg.Listen }

func (l *Listener) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !AuthOK(r, l.cfg.PSK) {
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid X-Peer-Token")
		return false
	}
	return true
}

func (l *Listener) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !l.requireAuth(w, r) {
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (l *Listener) handleManifest(w http.ResponseWriter, r *http.Request) {
	if !l.requireAuth(w, r) {
		return
	}
	cur := l.cfg.Provider()
	if cur == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no config")
		return
	}
	m := BuildManifest(l.cfg.Instance, 0, cur, l.cfg.peerName())
	writeJSON(w, m)
}

func (l *Listener) handleItems(w http.ResponseWriter, r *http.Request) {
	if !l.requireAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cur := l.cfg.Provider()
		if cur == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no config")
			return
		}
		env := FilterForExport(l.cfg.Instance, 0, cur, l.cfg.peerName())
		// since is a coarse pointer; we don't use it for filtering
		// (we always send the full snapshot) but we honor it in the
		// response so the client knows what we sent.
		if s := r.URL.Query().Get("since"); s != "" {
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				env.FromSeq = n
			}
		}
		writeJSON(w, env)
	case http.MethodPost:
		env, err := decodeEnvelope(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Quick reject if the envelope's clock is too far off.
		if l.cfg.Skew > 0 && !env.ServerTime.IsZero() {
			dt := time.Since(env.ServerTime)
			if dt < -l.cfg.Skew || dt > l.cfg.Skew {
				writeJSONError(w, http.StatusBadRequest,
					fmt.Sprintf("server_time out of window (skew %s, got %s)", l.cfg.Skew, dt))
				return
			}
		}
		if l.cfg.PushSink == nil {
			writeJSONError(w, http.StatusNotImplemented, "push sink not configured")
			return
		}
		res, err := l.cfg.PushSink(env, l.cfg.peerName())
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if l.cfg.Audit != nil {
			_ = l.cfg.Audit.Write(audit.Entry{
				Actor:   "peer:" + env.InstanceID,
				Action:  "peer-push-applied",
				Details: StringifyMerge(res),
			})
		}
		writeJSON(w, res)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// StringifyMerge produces a short, stable summary for audit logs.
func StringifyMerge(r MergeResult) string {
	if r.Added == 0 && r.Updated == 0 && r.Deleted == 0 && r.Conflicts == 0 && r.Skipped == 0 && r.Rejected == 0 {
		return "noop"
	}
	parts := []string{}
	if r.Added > 0 {
		parts = append(parts, "added="+itoa(r.Added))
	}
	if r.Updated > 0 {
		parts = append(parts, "updated="+itoa(r.Updated))
	}
	if r.Deleted > 0 {
		parts = append(parts, "deleted="+itoa(r.Deleted))
	}
	if r.Conflicts > 0 {
		parts = append(parts, "conflicts="+itoa(r.Conflicts))
	}
	if r.Skipped > 0 {
		parts = append(parts, "skipped="+itoa(r.Skipped))
	}
	if r.Rejected > 0 {
		parts = append(parts, "rejected="+itoa(r.Rejected))
	}
	return strings.Join(parts, " ")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
