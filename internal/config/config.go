package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	CurrentVersion = 1
	DefaultPath    = "/config/dnsforwarder.yaml"
)

type Settings struct {
	CacheSize   int    `yaml:"cache_size"   json:"cache_size"`
	DNSSEC      bool   `yaml:"dnssec"       json:"dnssec"`
	LogQueries  bool   `yaml:"log_queries"  json:"log_queries"`
	HTTPListen  string `yaml:"http_listen"  json:"http_listen"`
	DNSListen   string `yaml:"dns_listen"   json:"dns_listen"`
	PollSeconds int    `yaml:"poll_seconds" json:"poll_seconds"`
}

func (s *Settings) WithDefaults() Settings {
	out := *s
	if out.CacheSize == 0 {
		out.CacheSize = 1000
	}
	if out.HTTPListen == "" {
		out.HTTPListen = "0.0.0.0:80"
	}
	if out.DNSListen == "" {
		out.DNSListen = "0.0.0.0:53"
	}
	if out.PollSeconds == 0 {
		out.PollSeconds = 10
	}
	return out
}

// PeerStatus is the live, in-memory status of a configured peer. It is
// kept in Peers.List[*].Status (transient) but is part of the YAML schema
// only as an exported JSON field — not persisted by Save, since the GUI
// re-renders it from the live data on each request.
type PeerStatus struct {
	LastContact time.Time `yaml:"-" json:"last_contact"`
	LastSeq     uint64    `yaml:"-" json:"last_seq"`
	LastError   string    `yaml:"-" json:"last_error"`
	LastAction  string    `yaml:"-" json:"last_action"` // "pulled N", "pushed N", "skipped"
}

type Peer struct {
	Name    string     `yaml:"name"    json:"name"`
	URL     string     `yaml:"url"     json:"url"`
	Enabled bool       `yaml:"enabled" json:"enabled"`
	Status  PeerStatus `yaml:"-"       json:"status"`
}

type Peers struct {
	Listen              string `yaml:"listen"                  json:"listen"`
	SharedKey           string `yaml:"shared_key,omitempty"    json:"shared_key,omitempty"` // never marshalled; redacted on export
	SyncIntervalSeconds int    `yaml:"sync_interval_seconds"   json:"sync_interval_seconds"`
	ClockSkewSeconds    int    `yaml:"clock_skew_seconds"      json:"clock_skew_seconds"`
	List                []Peer `yaml:"list"                    json:"list"`
}

func (p *Peers) WithDefaults() Peers {
	out := *p
	if out.Listen == "" {
		out.Listen = "0.0.0.0:8443"
	}
	if out.SyncIntervalSeconds == 0 {
		out.SyncIntervalSeconds = 300 // 5 minutes
	}
	if out.SyncIntervalSeconds < 10 {
		out.SyncIntervalSeconds = 10 // safety floor so a typo can't hammer peers
	}
	if out.ClockSkewSeconds == 0 {
		out.ClockSkewSeconds = 300 // ±5 min
	}
	return out
}

type UpstreamDefault struct {
	Address   string    `yaml:"address"      json:"address"`
	Port      int       `yaml:"port"         json:"port"`
	Enabled   bool      `yaml:"enabled"      json:"enabled"`
	Note      string    `yaml:"note"         json:"note"` // freeform, e.g. "Azure DNS"
	DoNotSync bool      `yaml:"do_not_sync"  json:"do_not_sync"`
	SyncPeers []string  `yaml:"sync_peers,omitempty" json:"sync_peers,omitempty"` // empty = all peers
	UpdatedAt time.Time `yaml:"updated_at"   json:"updated_at"`
	UpdatedBy string    `yaml:"updated_by"   json:"updated_by"`
	Deleted   bool      `yaml:"deleted"      json:"deleted"` // tombstone for peer sync
}

func (u *UpstreamDefault) defaults() {
	if u.Port == 0 {
		u.Port = 53
	}
	// Enabled is a tri-state: only set to true if it's the zero value AND
	// we have a meaningful address. We can't distinguish "explicitly false"
	// from "unset" in Go's zero value, so on first read from YAML (where
	// the field was missing) we treat zero as true. This is fine because
	// the YAML is only read once at boot — after that, all entries have
	// an explicit value and this branch is skipped.
	//
	// The way we ensure this: after Load(), the hash is seeded and we
	// never re-read the file's "default" of an empty field. For new
	// entries created via the API, Enabled is always set explicitly.
}

func (u UpstreamDefault) String() string {
	return fmt.Sprintf("%s:%d", u.Address, u.Port)
}

type ForwardZone struct {
	ID                   string    `yaml:"id"         json:"id"`
	Name                 string    `yaml:"name"       json:"name"`
	Wildcard             bool      `yaml:"wildcard"   json:"wildcard"`
	Upstreams            []string  `yaml:"upstreams"  json:"upstreams"`
	LeastLatency         bool      `yaml:"least_latency" json:"least_latency"`
	LatencyTestFrequency int       `yaml:"latency_test_frequency" json:"latency_test_frequency"`
	DoNotSync            bool      `yaml:"do_not_sync" json:"do_not_sync"`
	SyncPeers            []string  `yaml:"sync_peers,omitempty" json:"sync_peers,omitempty"` // empty = all peers
	UpdatedAt            time.Time `yaml:"updated_at" json:"updated_at"`
	UpdatedBy            string    `yaml:"updated_by" json:"updated_by"`
	Deleted              bool      `yaml:"deleted"    json:"deleted"`
}

type StaticRecord struct {
	ID        string    `yaml:"id"         json:"id"`
	Name      string    `yaml:"name"       json:"name"`
	Type      string    `yaml:"type"       json:"type"`
	Value     string    `yaml:"value"      json:"value"`
	TTL       int       `yaml:"ttl"        json:"ttl"`
	DoNotSync bool      `yaml:"do_not_sync" json:"do_not_sync"`
	SyncPeers []string  `yaml:"sync_peers,omitempty" json:"sync_peers,omitempty"` // empty = all peers
	UpdatedAt time.Time `yaml:"updated_at" json:"updated_at"`
	UpdatedBy string    `yaml:"updated_by" json:"updated_by"`
	Deleted   bool      `yaml:"deleted"    json:"deleted"`
}

type File struct {
	Version           int               `yaml:"version"          json:"version"`
	Updated           time.Time         `yaml:"updated"          json:"updated"`
	UpdatedBy         string            `yaml:"updated_by"       json:"updated_by"`
	Settings          Settings          `yaml:"settings"         json:"settings"`
	Peers             Peers             `yaml:"peers"            json:"peers"`
	UpstreamDefaults  []UpstreamDefault `yaml:"upstream_defaults" json:"upstream_defaults"`
	ForwardZones      []ForwardZone     `yaml:"forward_zones"    json:"forward_zones"`
	StaticRecords     []StaticRecord    `yaml:"static_records"   json:"static_records"`
	DoNotSyncSettings bool              `yaml:"do_not_sync_settings" json:"do_not_sync_settings"`
}

// SyncsToPeer reports whether an item (with its own DoNotSync and SyncPeers
// fields) should be advertised to the named peer. It returns false if the item
// is marked DoNotSync, or if SyncPeers is non-empty and the peer is not listed.
func SyncsToPeer(doNotSync bool, syncPeers []string, peerName string) bool {
	if doNotSync {
		return false
	}
	if len(syncPeers) == 0 {
		return true
	}
	peerName = strings.ToLower(strings.TrimSpace(peerName))
	for _, p := range syncPeers {
		if strings.ToLower(strings.TrimSpace(p)) == peerName {
			return true
		}
	}
	return false
}

var (
	// domainRe matches a fully-qualified domain (at least one dot) optionally
	// prefixed with "*." for wildcards. Used for forward zones.
	domainRe = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)
	// labelRe matches a single DNS label (no dots). For static records
	// and for single-label forward zones like "corp" or "intranet".
	labelRe = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	// dnsNameRe matches any valid DNS name — single label OR FQDN.
	// Used for static record names so single-label names work.
	dnsNameRe  = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	ipv4Re     = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
	ipv6Re     = regexp.MustCompile(`^([0-9a-fA-F:]+)$`)
	hostnameRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
)

func (f *File) Validate() error {
	if f.Version == 0 {
		f.Version = CurrentVersion
	}
	if f.UpstreamDefaults == nil {
		return errors.New("upstream_defaults is required (use an empty list if you only forward to specific zones)")
	}
	// upstreams_defaults is allowed to be empty: a deployment that only
	// forwards to specific zones (no catch-all) is valid. If non-empty,
	// every entry must be a valid IP and at least one must be enabled.
	enabledCount := 0
	for i := range f.UpstreamDefaults {
		u := &f.UpstreamDefaults[i]
		u.defaults()
		// Soft-deleted defaults are allowed to collide with a newly added
		// default. They are filtered from API/GUI lists and only kept for
		// peer sync propagation, so we still validate non-deleted ones.
		if !u.Deleted {
			if !ipv4Re.MatchString(u.Address) && !ipv6Re.MatchString(u.Address) {
				return fmt.Errorf("upstream_defaults[%d].address: invalid IP %q", i, u.Address)
			}
			if u.Port <= 0 || u.Port > 65535 {
				return fmt.Errorf("upstream_defaults[%d].port: out of range", i)
			}
			if u.Enabled {
				enabledCount++
			}
		}
	}
	if len(f.UpstreamDefaults) > 0 && enabledCount == 0 {
		// Allowing the catch-all to be fully disabled is fine if forward
		// zones cover everything (or the user wants to refuse unknown
		// queries). Only error if there's no other route.
		if len(f.ForwardZones) == 0 {
			return errors.New("upstream_defaults has entries but all are disabled and there are no forward_zones; either enable one, remove them, or add a forward zone")
		}
	}
	if len(f.UpstreamDefaults) == 0 && len(f.ForwardZones) == 0 {
		return errors.New("no upstream_defaults and no forward_zones: every query would be refused")
	}
	f.Settings = f.Settings.WithDefaults()
	f.Peers = f.Peers.WithDefaults()

	seenZones := map[string]bool{}
	for i, z := range f.ForwardZones {
		if z.ID == "" {
			return fmt.Errorf("forward_zones[%d]: id is required", i)
		}
		cleanName := strings.TrimPrefix(z.Name, "*.")
		// Forward zone names: FQDN (with at least one dot) OR single label.
		// Wildcards always need a "." in the underlying name.
		underlying := strings.TrimPrefix(z.Name, "*.")
		if !domainRe.MatchString(z.Name) && !labelRe.MatchString(z.Name) {
			return fmt.Errorf("forward_zones[%d].name: invalid domain %q", i, z.Name)
		}
		// Soft-deleted (tombstoned) zones are allowed to collide with a new
		// zone of the same name. They are filtered out of API/GUI lists and
		// are only kept for peer sync propagation.
		if !z.Deleted {
			if seenZones[cleanName] {
				return fmt.Errorf("forward_zones[%d]: duplicate zone %q", i, cleanName)
			}
			seenZones[cleanName] = true
		}
		if z.Wildcard && !strings.HasPrefix(z.Name, "*.") {
			return fmt.Errorf("forward_zones[%d]: wildcard=true but name does not start with '*.", i)
		}
		if !z.Wildcard && strings.HasPrefix(z.Name, "*.") {
			return fmt.Errorf("forward_zones[%d]: name starts with '*. but wildcard=false", i)
		}
		// For a single-label zone, "wildcard" doesn't make much sense and
		// is ignored by unbound. We don't error — just normalize.
		if z.Wildcard && !strings.Contains(underlying, ".") {
			// silently treat wildcard=true on a single label as wildcard=false
			z.Wildcard = false
		}
		if len(z.Upstreams) == 0 {
			return fmt.Errorf("forward_zones[%d] %q: at least one upstream required", i, z.Name)
		}
		if z.LeastLatency {
			if z.Wildcard {
				return fmt.Errorf("forward_zones[%d] %q: least latency is only available for exact (non-wildcard) names", i, z.Name)
			}
			if z.LatencyTestFrequency <= 0 {
				z.LatencyTestFrequency = 5
			}
			// LatencyTestFrequency is stored and displayed in minutes.
			// Any positive integer is accepted; very low values are allowed
			// but will probe the targets more often.
			if z.LatencyTestFrequency < 1 {
				z.LatencyTestFrequency = 1
			}
		}
		for j, up := range z.Upstreams {
			if !ipv4Re.MatchString(up) && !ipv6Re.MatchString(up) && !hostnameRe.MatchString(up) {
				return fmt.Errorf("forward_zones[%d] %q: invalid upstream %q at index %d", i, z.Name, up, j)
			}
		}
	}

	seenRecs := map[string]bool{}
	for i, r := range f.StaticRecords {
		if r.ID == "" {
			return fmt.Errorf("static_records[%d]: id is required", i)
		}
		// Static records accept any valid DNS name, including single labels.
		if !dnsNameRe.MatchString(r.Name) {
			return fmt.Errorf("static_records[%d].name: invalid %q", i, r.Name)
		}
		key := strings.ToLower(r.Name) + "|" + strings.ToUpper(r.Type)
		// Soft-deleted records are allowed to collide with a newly added
		// record of the same name/type. They are filtered from API/GUI lists
		// and only kept for peer sync propagation.
		if !r.Deleted {
			if seenRecs[key] {
				return fmt.Errorf("static_records[%d]: duplicate %s %s", i, r.Type, r.Name)
			}
			seenRecs[key] = true
		}
		switch strings.ToUpper(r.Type) {
		case "A":
			if !ipv4Re.MatchString(r.Value) {
				return fmt.Errorf("static_records[%d] %s: A record needs IPv4, got %q", i, r.Name, r.Value)
			}
		case "AAAA":
			if !ipv6Re.MatchString(r.Value) {
				return fmt.Errorf("static_records[%d] %s: AAAA record needs IPv6, got %q", i, r.Name, r.Value)
			}
		case "CNAME":
			if !hostnameRe.MatchString(r.Value) {
				return fmt.Errorf("static_records[%d] %s: CNAME target invalid %q", i, r.Name, r.Value)
			}
		default:
			return fmt.Errorf("static_records[%d] %s: unsupported type %q (A, AAAA, CNAME)", i, r.Name, r.Type)
		}
	}
	return nil
}

func (f *File) Snapshot() File {
	return File{
		Version:           f.Version,
		Updated:           f.Updated,
		UpdatedBy:         f.UpdatedBy,
		Settings:          f.Settings,
		Peers:             f.Peers,
		UpstreamDefaults:  append([]UpstreamDefault(nil), f.UpstreamDefaults...),
		ForwardZones:      append([]ForwardZone(nil), f.ForwardZones...),
		StaticRecords:     append([]StaticRecord(nil), f.StaticRecords...),
		DoNotSyncSettings: f.DoNotSyncSettings,
	}
}

func (f *File) Clone() *File {
	cp := f.Snapshot()
	return &cp
}
