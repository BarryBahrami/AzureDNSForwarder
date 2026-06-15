package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var ErrLocked = errors.New("config is locked by another writer")

type Store struct {
	path string

	mu        sync.Mutex
	current   *File
	hash      string
	loadErr   error

	onSaveMu sync.RWMutex
	onSave   func()

	onApplyMu sync.RWMutex
	onApply   func()
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Path() string { return s.path }

func (s *Store) Current() *File {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *Store) Hash() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hash
}

func (s *Store) LoadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			f := DefaultConfig()
			SeedFromEnv(f)
			f.Updated = time.Now().UTC()
			f.UpdatedBy = "bootstrap"
			if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
				return err
			}
			out, mErr := yaml.Marshal(f)
			if mErr != nil {
				return mErr
			}
			if wErr := os.WriteFile(s.path, out, 0o644); wErr != nil {
				return wErr
			}
			// Seed the in-memory hash with the hash of the bootstrap file
			// we just wrote, so the poller does not fire a redundant reload
			// on its first tick.
			sum := sha256.Sum256(out)
			s.setCurrent(f, hex.EncodeToString(sum[:]))
			s.loadErr = nil
			return nil
		}
		s.loadErr = err
		return err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		s.loadErr = fmt.Errorf("parse: %w", err)
		return s.loadErr
	}
	SeedFromEnv(&f)
	if err := f.Validate(); err != nil {
		s.loadErr = fmt.Errorf("validate: %w", err)
		return s.loadErr
	}
	sum := sha256.Sum256(data)
	s.setCurrent(&f, hex.EncodeToString(sum[:]))
	s.loadErr = nil
	return nil
}

func (s *Store) setCurrent(f *File, hash string) {
	s.mu.Lock()
	s.current = f
	s.hash = hash
	s.mu.Unlock()
}

// SetOnSave registers a hook that fires after every successful Save.
// Used by the peer sync engine to push the new config to peers.
func (s *Store) SetOnSave(fn func()) {
	s.onSaveMu.Lock()
	s.onSave = fn
	s.onSaveMu.Unlock()
}

func (s *Store) fireOnSave() {
	s.onSaveMu.RLock()
	fn := s.onSave
	s.onSaveMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// SetOnApply registers a hook that fires after every successful Save
// to ask the watcher to reload unbound immediately. The server's
// afterSave hook does this for HTTP-driven saves; we also want it for
// peer-driven saves that don't go through the server.
func (s *Store) SetOnApply(fn func()) {
	s.onApplyMu.Lock()
	s.onApply = fn
	s.onApplyMu.Unlock()
}

func (s *Store) fireOnApply() {
	s.onApplyMu.RLock()
	fn := s.onApply
	s.onApplyMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// ReloadIfChanged reads the file, returns true if hash differs and parses ok.
// Returns false on no-change, parse error, or no-change.
func (s *Store) ReloadIfChanged() (changed bool, err error) {
	data, rerr := os.ReadFile(s.path)
	if rerr != nil {
		return false, rerr
	}
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	s.mu.Lock()
	cur := s.hash
	s.mu.Unlock()
	if h == cur {
		return false, nil
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return true, fmt.Errorf("parse: %w", err)
	}
	SeedFromEnv(&f)
	if err := f.Validate(); err != nil {
		return true, fmt.Errorf("validate: %w", err)
	}
	s.setCurrent(&f, h)
	return true, nil
}

type SaveOptions struct {
	Actor  string
	Mutate func(*File) error
}

// Save acquires an exclusive flock, applies mutate, validates, atomic-writes,
// and releases the lock. Returns ErrLocked if another writer holds the lock.
func (s *Store) Save(opts SaveOptions) (*File, error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lockPath := s.path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	defer lf.Close()

	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	// Note: we manually unlock (below) before invoking the on-save hook,
	// because the hook may want to acquire the lock itself (e.g. when it
	// pushes a change to a peer and the peer reflects the change back).
	unlock := func() error { return syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }

	cur := s.Current()
	if cur == nil {
		cur = DefaultConfig()
	}
	draft := cur.Clone()
	if opts.Mutate != nil {
		if err := opts.Mutate(draft); err != nil {
			_ = unlock()
			return nil, err
		}
	}
	draft.Updated = time.Now().UTC()
	draft.UpdatedBy = opts.Actor
	draft.Version = CurrentVersion
	stampItems(draft, opts.Actor)
	if err := draft.Validate(); err != nil {
		_ = unlock()
		return nil, err
	}
	data, err := yaml.Marshal(draft)
	if err != nil {
		_ = unlock()
		return nil, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = unlock()
		return nil, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = unlock()
		return nil, err
	}
	sum := sha256.Sum256(data)
	s.setCurrent(draft, hex.EncodeToString(sum[:]))
	if err := unlock(); err != nil {
		return nil, err
	}
	// Fire the on-save hook AFTER the lock is released so the hook can
	// safely take the lock again (e.g. to push to peers).
	s.fireOnSave()
	s.fireOnApply()
	return draft, nil
}

func DefaultConfig() *File {
	now := time.Now().UTC()
	return &File{
		Version: CurrentVersion,
		Settings: Settings{
			CacheSize:   1000,
			DNSSEC:      false,
			LogQueries:  false,
			HTTPListen:  "0.0.0.0:80",
			DNSListen:   "0.0.0.0:53",
			PollSeconds: 10,
		},
		Peers: Peers{
			Listen:              "0.0.0.0:8443",
			SyncIntervalSeconds: 300,
			ClockSkewSeconds:    300,
		},
		UpstreamDefaults: []UpstreamDefault{
			{Address: "168.63.129.16", Port: 53, Enabled: true, Note: "Azure DNS", UpdatedAt: now, UpdatedBy: "bootstrap"},
		},
	}
}

// SeedFromEnv applies any PEER_* environment variables to f, using a
// "no-clobber" rule: an existing value in f always wins. Env is bootstrap.
//
//	PEER_LISTEN             - listen address for the peer HTTP server
//	PEER_SHARED_KEY         - preshared key (X-Peer-Token header)
//	PEER_SHARED_KEY_FILE    - path to a file containing the preshared key
//	PEERS_INITIAL           - comma-separated list of peer URLs
//
// Note: the docker-compose PEERING_PORT (the WireGuard UDP port) is not
// consumed here; it belongs to the WireGuard container, not this app.
func SeedFromEnv(f *File) {
	if v, ok := os.LookupEnv("PEER_LISTEN"); ok && v != "" && f.Peers.Listen == "" {
		f.Peers.Listen = v
	}
	// PSK: env var takes precedence; otherwise try the *_FILE form.
	if f.Peers.SharedKey == "" {
		if v, ok := os.LookupEnv("PEER_SHARED_KEY"); ok && v != "" {
			f.Peers.SharedKey = v
		} else if path, ok := os.LookupEnv("PEER_SHARED_KEY_FILE"); ok && path != "" {
			if data, err := os.ReadFile(path); err == nil {
				key := strings.TrimSpace(string(data))
				if key != "" {
					f.Peers.SharedKey = key
				}
			}
		}
	}
	if v, ok := os.LookupEnv("PEERS_INITIAL"); ok && v != "" && len(f.Peers.List) == 0 {
		for _, raw := range strings.Split(v, ",") {
			url := strings.TrimSpace(raw)
			if url == "" {
				continue
			}
			name := peerNameFromURL(url)
			f.Peers.List = append(f.Peers.List, Peer{
				Name:    name,
				URL:     url,
				Enabled: true,
			})
		}
	}
}

func peerNameFromURL(u string) string {
	// Strip scheme, take host:port, replace dots/colons for a label.
	s := u
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, ".", "-")
	if s == "" {
		return "peer"
	}
	return s
}

// stampItems sets UpdatedAt/UpdatedBy on any mutable item that doesn't
// already have them. Items received from peers (via Merge) carry their
// own timestamp; items added locally via the API get the current time
// and the saving actor. This is the bookkeeping that makes LWW work.
func stampItems(f *File, actor string) {
	now := time.Now().UTC()
	for i := range f.UpstreamDefaults {
		if f.UpstreamDefaults[i].UpdatedAt.IsZero() {
			f.UpstreamDefaults[i].UpdatedAt = now
		}
		if f.UpstreamDefaults[i].UpdatedBy == "" {
			f.UpstreamDefaults[i].UpdatedBy = actor
		}
	}
	for i := range f.ForwardZones {
		if f.ForwardZones[i].UpdatedAt.IsZero() {
			f.ForwardZones[i].UpdatedAt = now
		}
		if f.ForwardZones[i].UpdatedBy == "" {
			f.ForwardZones[i].UpdatedBy = actor
		}
	}
	for i := range f.StaticRecords {
		if f.StaticRecords[i].UpdatedAt.IsZero() {
			f.StaticRecords[i].UpdatedAt = now
		}
		if f.StaticRecords[i].UpdatedBy == "" {
			f.StaticRecords[i].UpdatedBy = actor
		}
	}
}

// PeerNameFromURL is the exported form used by the API and GUI for
// auto-naming a peer from its URL when the operator leaves the name blank.
func PeerNameFromURL(u string) string { return peerNameFromURL(u) }
