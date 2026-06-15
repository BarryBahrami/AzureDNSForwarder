// Package sync implements peer-to-peer configuration exchange between
// dnsforwarderd instances. It is a small, last-writer-wins CRDT-lite:
//
//   - every mutable item carries UpdatedAt (monotonic per writer)
//   - tombstones (Deleted=true) propagate so deletes survive
//   - per-item DoNotSync is enforced at the source (a peer never sees
//     items the originating instance marked private)
//   - per-instance Seq is a coarse "anything changed since N" pointer
//   - the local instance is the source of truth for items we authored;
//     remote items are accepted only if newer than our local copy
package sync

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/config"
)

// Manifest is the lightweight "what do you have?" summary returned by
// GET /peer/v1/manifest. The puller uses it to decide whether a full
// GET /peer/v1/items is warranted.
type Manifest struct {
	InstanceID string    `json:"instance_id"`
	Seq        uint64    `json:"seq"`
	Counts     ItemCount `json:"counts"`
	ServerTime time.Time `json:"server_time"`
}

type ItemCount struct {
	UpstreamDefaults int `json:"upstream_defaults"`
	ForwardZones     int `json:"forward_zones"`
	StaticRecords    int `json:"static_records"`
}

// Envelope is the wire format for both push and pull. Items is grouped
// by kind so the receiver can update the right slice.
type Envelope struct {
	InstanceID string                  `json:"instance_id"`
	FromSeq    uint64                  `json:"from_seq"`     // last seq the sender had when it built this
	ServerTime time.Time               `json:"server_time"`
	UpstreamDefaults []config.UpstreamDefault `json:"upstream_defaults,omitempty"`
	ForwardZones     []config.ForwardZone     `json:"forward_zones,omitempty"`
	StaticRecords    []config.StaticRecord    `json:"static_records,omitempty"`
	// Settings is sent as a single block; the receiver either takes it
	// whole or ignores it (if its DoNotSyncSettings is set).
	Settings *config.Settings `json:"settings,omitempty"`
}

// AuthOK returns true iff the X-Peer-Token header matches the configured
// PSK using a constant-time compare. Empty PSK = disabled: no auth, no
// requests accepted (fail closed).
func AuthOK(r *http.Request, want string) bool {
	if want == "" {
		return false
	}
	got := r.Header.Get("X-Peer-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// TLSConfig returns a tls.Config that accepts any client cert and listens
// for any peer. In production this is the inner side of a WireGuard tunnel;
// in dev we use it over plaintext HTTP when wg.IsEncrypted returns false.
//
// The Config is suitable for both server and client; the client side just
// doesn't verify the peer (intentional for the WG-wrapped case where
// encryption is at the tunnel layer).
func TLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // see note above
		MinVersion:         tls.VersionTLS12,
	}
}

// BuildManifest produces the lightweight summary for the manifest endpoint.
// It uses the store's current seq counter and counts of non-tombstoned,
// non-DoNotSync items that are eligible for the named peer.
func BuildManifest(instanceID string, seq uint64, f *config.File, peerName string) Manifest {
	m := Manifest{
		InstanceID: instanceID,
		Seq:        seq,
		ServerTime: time.Now().UTC(),
	}
	for _, u := range f.UpstreamDefaults {
		if !u.Deleted && config.SyncsToPeer(u.DoNotSync, u.SyncPeers, peerName) {
			m.Counts.UpstreamDefaults++
		}
	}
	for _, z := range f.ForwardZones {
		if !z.Deleted && config.SyncsToPeer(z.DoNotSync, z.SyncPeers, peerName) {
			m.Counts.ForwardZones++
		}
	}
	for _, r := range f.StaticRecords {
		if !r.Deleted && config.SyncsToPeer(r.DoNotSync, r.SyncPeers, peerName) {
			m.Counts.StaticRecords++
		}
	}
	return m
}

// FilterForExport returns an Envelope containing only items that are
// eligible to be sent to the named peer (not DoNotSync, SyncPeers empty or
// includes the peer, and not deleted except as tombstones that still need
// to propagate). Settings is included only when DoNotSyncSettings is false.
func FilterForExport(instanceID string, seq uint64, f *config.File, peerName string) Envelope {
	env := Envelope{
		InstanceID: instanceID,
		FromSeq:    seq,
		ServerTime: time.Now().UTC(),
	}
	for _, u := range f.UpstreamDefaults {
		if !config.SyncsToPeer(u.DoNotSync, u.SyncPeers, peerName) {
			continue
		}
		env.UpstreamDefaults = append(env.UpstreamDefaults, u)
	}
	for _, z := range f.ForwardZones {
		if !config.SyncsToPeer(z.DoNotSync, z.SyncPeers, peerName) {
			continue
		}
		env.ForwardZones = append(env.ForwardZones, z)
	}
	for _, r := range f.StaticRecords {
		if !config.SyncsToPeer(r.DoNotSync, r.SyncPeers, peerName) {
			continue
		}
		env.StaticRecords = append(env.StaticRecords, r)
	}
	if !f.DoNotSyncSettings {
		s := f.Settings
		env.Settings = &s
	}
	return env
}

// MergeResult is the outcome of a single Merge call. The peer loop uses
// it to update per-peer status and the audit log.
type MergeResult struct {
	Added     int
	Updated   int
	Deleted   int
	Skipped   int
	Conflicts int // same name, different value, LWW resolved
	Rejected  int // clock skew or schema invalid
}

// Merge applies an incoming envelope to the local config using LWW on
// UpdatedAt. Items that already match (by ID and timestamp) are skipped.
// Conflicts where the local copy is newer are kept; the incoming is
// discarded (and counted as a Conflict for the audit log).
//
// The function does NOT persist. The caller is expected to do
// store.Save with the resulting draft (or use the direct-mutation form).
func Merge(local *config.File, env Envelope, skew time.Duration) MergeResult {
	var r MergeResult
	now := time.Now().UTC()

	// Upstream defaults: matched by (Address, Port).
	{
		idx := make(map[string]int, len(local.UpstreamDefaults))
		for i, u := range local.UpstreamDefaults {
			idx[upstreamKey(u.Address, u.Port)] = i
		}
		for _, in := range env.UpstreamDefaults {
			if in.DoNotSync {
				r.Skipped++
				continue
			}
			if skew > 0 && !in.UpdatedAt.IsZero() {
				dt := now.Sub(in.UpdatedAt)
				if dt < -skew || dt > skew {
					r.Rejected++
					continue
				}
			}
			k := upstreamKey(in.Address, in.Port)
			i, ok := idx[k]
			if !ok {
				if in.Deleted {
					// tombstone for something we don't have: ignore
					r.Skipped++
					continue
				}
				local.UpstreamDefaults = append(local.UpstreamDefaults, in)
				idx[k] = len(local.UpstreamDefaults) - 1
				r.Added++
				continue
			}
			cur := &local.UpstreamDefaults[i]
			if in.Deleted && !cur.Deleted {
				if cur.UpdatedAt.After(in.UpdatedAt) {
					r.Conflicts++
					continue
				}
				*cur = in
				r.Deleted++
				continue
			}
			if cur.Deleted && !in.Deleted {
				if in.UpdatedAt.After(cur.UpdatedAt) {
					*cur = in
					r.Updated++
				} else {
					r.Conflicts++
				}
				continue
			}
			if cur.UpdatedAt.After(in.UpdatedAt) {
				r.Conflicts++
				continue
			}
			if cur.UpdatedAt.Equal(in.UpdatedAt) && cur.Enabled == in.Enabled && cur.Note == in.Note {
				r.Skipped++
				continue
			}
			r.Conflicts++ // value change is a conflict that LWW resolved
			*cur = in
		}
	}

	// Forward zones: matched by ID.
	{
		idx := make(map[string]int, len(local.ForwardZones))
		for i, z := range local.ForwardZones {
			idx[z.ID] = i
		}
		for _, in := range env.ForwardZones {
			if in.DoNotSync {
				r.Skipped++
				continue
			}
			if skew > 0 && !in.UpdatedAt.IsZero() {
				dt := now.Sub(in.UpdatedAt)
				if dt < -skew || dt > skew {
					r.Rejected++
					continue
				}
			}
			i, ok := idx[in.ID]
			if !ok {
				if in.Deleted {
					r.Skipped++
					continue
				}
				local.ForwardZones = append(local.ForwardZones, in)
				idx[in.ID] = len(local.ForwardZones) - 1
				r.Added++
				continue
			}
			cur := &local.ForwardZones[i]
			if in.Deleted && !cur.Deleted {
				if cur.UpdatedAt.After(in.UpdatedAt) {
					r.Conflicts++
					continue
				}
				*cur = in
				r.Deleted++
				continue
			}
			if cur.Deleted && !in.Deleted {
				if in.UpdatedAt.After(cur.UpdatedAt) {
					*cur = in
					r.Updated++
				} else {
					r.Conflicts++
				}
				continue
			}
			if cur.UpdatedAt.After(in.UpdatedAt) {
				r.Conflicts++
				continue
			}
			if cur.UpdatedAt.Equal(in.UpdatedAt) {
				r.Skipped++
				continue
			}
			r.Conflicts++
			*cur = in
		}
	}

	// Static records: matched by ID.
	{
		idx := make(map[string]int, len(local.StaticRecords))
		for i, s := range local.StaticRecords {
			idx[s.ID] = i
		}
		for _, in := range env.StaticRecords {
			if in.DoNotSync {
				r.Skipped++
				continue
			}
			if skew > 0 && !in.UpdatedAt.IsZero() {
				dt := now.Sub(in.UpdatedAt)
				if dt < -skew || dt > skew {
					r.Rejected++
					continue
				}
			}
			i, ok := idx[in.ID]
			if !ok {
				if in.Deleted {
					r.Skipped++
					continue
				}
				local.StaticRecords = append(local.StaticRecords, in)
				idx[in.ID] = len(local.StaticRecords) - 1
				r.Added++
				continue
			}
			cur := &local.StaticRecords[i]
			if in.Deleted && !cur.Deleted {
				if cur.UpdatedAt.After(in.UpdatedAt) {
					r.Conflicts++
					continue
				}
				*cur = in
				r.Deleted++
				continue
			}
			if cur.Deleted && !in.Deleted {
				if in.UpdatedAt.After(cur.UpdatedAt) {
					*cur = in
					r.Updated++
				} else {
					r.Conflicts++
				}
				continue
			}
			if cur.UpdatedAt.After(in.UpdatedAt) {
				r.Conflicts++
				continue
			}
			if cur.UpdatedAt.Equal(in.UpdatedAt) {
				r.Skipped++
				continue
			}
			r.Conflicts++
			*cur = in
		}
	}

	// Settings block: take only if incoming is newer. Local always wins
	// when DoNotSyncSettings is set locally.
	if env.Settings != nil && !local.DoNotSyncSettings {
		if local.Updated.Before(env.ServerTime) {
			local.Settings = *env.Settings
		}
	}

	return r
}

func upstreamKey(addr string, port int) string {
	return strings.ToLower(addr) + ":" + itoa(port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// SortByUpdated sorts the envelope items by UpdatedAt ascending so that
// the receiver applies them in a deterministic order. Used in tests.
func SortByUpdated(env *Envelope) {
	less := func(a, b time.Time) bool { return a.Before(b) }
	sort.SliceStable(env.UpstreamDefaults, func(i, j int) bool { return less(env.UpstreamDefaults[i].UpdatedAt, env.UpstreamDefaults[j].UpdatedAt) })
	sort.SliceStable(env.ForwardZones, func(i, j int) bool { return less(env.ForwardZones[i].UpdatedAt, env.ForwardZones[j].UpdatedAt) })
	sort.SliceStable(env.StaticRecords, func(i, j int) bool { return less(env.StaticRecords[i].UpdatedAt, env.StaticRecords[j].UpdatedAt) })
}

// JSONError is a structured 4xx/5xx body for the peer API.
type JSONError struct {
	Error string `json:"error"`
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(JSONError{Error: msg})
}

// decodeEnvelope parses a request body into an Envelope and validates
// the bare minimum (non-nil pointer to a struct).
func decodeEnvelope(r *http.Request) (Envelope, error) {
	var env Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		return env, fmt.Errorf("invalid json: %w", err)
	}
	return env, nil
}

// errBadRequest is exported for use by handlers.
var errBadRequest = errors.New("bad request")
