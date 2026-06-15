// Package proxy implements a loopback DNS proxy used by the Azure DNS
// Forwarder to provide "least latency" responses for exact-match forward
// zones.
//
// When a forward zone has LeastLatency enabled, unbound is configured to
// forward all matching queries to this proxy. The proxy periodically
// resolves the zone name through the configured upstream(s), measures the
// latency (via TCP handshake to port 53/443) to every address in the answer,
// and caches the lowest-latency targets. When a real query arrives, the
// proxy returns an answer containing only the best targets (all targets
// tied at the lowest latency).
//
// Sync partners receive the zone configuration (including the LeastLatency
// flag and test frequency) but each peer runs its own independent latency
// tests, so returned answers can differ per peer.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/barry/AzureDNSForwarder/internal/config"
)

const (
	DefaultListen     = "127.0.0.1:15353"
	DefaultListenPort = 15353
)

// Server is a DNS server that serves pre-computed, latency-filtered answers
// for exact-match forward zones that have LeastLatency enabled.
type Server struct {
	// Listen is the loopback UDP address to bind. Defaults to DefaultListen.
	Listen string

	logger *slog.Logger
	config func() *config.File

	mu      sync.RWMutex
	dns     *dns.Server
	zones   map[string]*zoneState
	ticker  *time.Ticker
	done    chan struct{}
	started bool
}

type zoneState struct {
	Name     string
	Upstream string
	Interval time.Duration
	Records  []latencyRecord
	Updated  time.Time
	Err      string
}

type latencyRecord struct {
	Target string // the FQDN (for CNAMEs) or IP string
	Type   uint16 // dns.TypeA, dns.TypeAAAA, dns.TypeCNAME
	Value  string // IP or canonical name for CNAME
	TTL    uint32
	RTT    time.Duration // zero means unreachable / not measured
}

func New(cfg func() *config.File, logger *slog.Logger) *Server {
	return &Server{
		Listen: DefaultListen,
		logger: logger,
		config: cfg,
		zones:  make(map[string]*zoneState),
		done:   make(chan struct{}),
	}
}

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	listen := s.Listen
	if listen == "" {
		listen = DefaultListen
	}
	handler := dns.NewServeMux()
	handler.HandleFunc(".", s.handle)
	s.dns = &dns.Server{
		Addr:      listen,
		Net:       "udp",
		Handler:   handler,
		ReusePort: true,
	}

	s.rebuildZones()
	// Per-zone probe tick is 1 minute; each zone only re-probes when its own
	// interval has elapsed, so short intervals are respected without
	// hammering long-interval zones.
	s.ticker = time.NewTicker(time.Minute)
	s.started = true

	go s.refreshLoop()
	go func() {
		if err := s.dns.ListenAndServe(); err != nil && !isClosedErr(err) {
			s.logger.Error("latency proxy server exited", "err", err)
		}
	}()

	s.logger.Info("latency proxy started", "addr", listen)
	return nil
}

func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.done)
	dns := s.dns
	s.mu.Unlock()

	if dns != nil {
		if err := dns.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}

// RefreshNow rebuilds the set of latency-aware zones and immediately runs a
// probe pass. It is safe for concurrent use.
func (s *Server) RefreshNow() {
	s.mu.Lock()
	s.rebuildZones()
	zones := s.zoneSnapshot()
	s.mu.Unlock()

	s.probeAll(zones)
}

func (s *Server) refreshLoop() {
	// Immediate first pass so zones don't sit unprobed for a minute after
	// unbound has already been pointed at the proxy.
	s.runProbePass()

	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			s.runProbePass()
		}
	}
}

func (s *Server) runProbePass() {
	s.mu.Lock()
	s.rebuildZones()
	zones := s.zoneSnapshot()
	s.mu.Unlock()

	var wg sync.WaitGroup
	now := time.Now().UTC()
	for _, z := range zones {
		if !z.Updated.IsZero() && now.Sub(z.Updated) < z.Interval {
			continue
		}
		wg.Add(1)
		go func(z *zoneState) {
			defer wg.Done()
			s.probeZone(context.Background(), z)
		}(z)
	}
	wg.Wait()
}

func (s *Server) zoneSnapshot() []*zoneState {
	out := make([]*zoneState, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, z)
	}
	return out
}

// rebuildZones updates the internal zone map from the current config. Zones
// that are no longer least-latency enabled are dropped; zones that changed
// upstream/frequency are replaced so the next probe uses fresh settings.
func (s *Server) rebuildZones() {
	cur := s.config()
	if cur == nil {
		s.zones = make(map[string]*zoneState)
		return
	}

	newZones := make(map[string]*zoneState)
	for _, z := range cur.ForwardZones {
		if z.Deleted || !z.LeastLatency || z.Wildcard {
			continue
		}
		if len(z.Upstreams) == 0 {
			continue
		}
		fqdn := dns.Fqdn(z.Name)
		interval := time.Duration(z.LatencyTestFrequency) * time.Minute
		if interval <= 0 {
			interval = 5 * time.Minute
		}

		// Prefer the first upstream for the recursive probe. The upstream
		// may return multiple records; we then ping each returned record.
		upstream := z.Upstreams[0]
		if !strings.Contains(upstream, ":") {
			upstream += ":53"
		}

		if old, ok := s.zones[fqdn]; ok {
			if old.Upstream == upstream && old.Interval == interval {
				newZones[fqdn] = old
				continue
			}
		}
		newZones[fqdn] = &zoneState{
			Name:     fqdn,
			Upstream: upstream,
			Interval: interval,
		}
	}
	s.zones = newZones
}

func (s *Server) probeAll(zones []*zoneState) {
	var wg sync.WaitGroup
	for _, z := range zones {
		wg.Add(1)
		go func(z *zoneState) {
			defer wg.Done()
			s.probeZone(context.Background(), z)
		}(z)
	}
	wg.Wait()
}

func (s *Server) probeZone(ctx context.Context, z *zoneState) {
	s.logger.Debug("latency probe", "zone", z.Name, "upstream", z.Upstream)

	records, err := resolveTarget(ctx, z.Name, z.Upstream)
	if err != nil {
		s.mu.Lock()
		z.Err = err.Error()
		z.Updated = time.Now().UTC()
		s.mu.Unlock()
		s.logger.Warn("latency probe failed", "zone", z.Name, "upstream", z.Upstream, "err", err)
		return
	}

	// Concurrent ping of every IP target. CNAMEs without a final A/AAAA
	// target are not ranked, but are still returned if they are the only
	// answer.
	ips := make([]string, 0, len(records))
	for i := range records {
		r := &records[i]
		if r.Type == dns.TypeA || r.Type == dns.TypeAAAA {
			ips = append(ips, r.Value)
		}
	}
	rttMap, bestRTT := concurrentPinger(ctx, ips)
	for i := range records {
		r := &records[i]
		if r.Type == dns.TypeA || r.Type == dns.TypeAAAA {
			if v, ok := rttMap[r.Value]; ok {
				r.RTT = v
			}
		}
	}

	// If every ping failed, keep all records so queries still resolve. If
	// at least one ping succeeded, discard unreachable records.
	keepAll := bestRTT <= 0
	if !keepAll {
		filtered := make([]latencyRecord, 0, len(records))
		for _, r := range records {
			if r.Type == dns.TypeCNAME || r.RTT == bestRTT {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}

	s.mu.Lock()
	z.Records = records
	z.Err = ""
	z.Updated = time.Now().UTC()
	s.mu.Unlock()

	s.logger.Debug("latency probe done", "zone", z.Name, "records", len(records), "best_ms", bestRTT.Milliseconds())
}

// resolveTarget asks the configured upstream for the zone name and returns
// any A / AAAA / CNAME records found in the answer section. It follows CNAME
// chains present in the answer but does not do recursive resolution beyond
// the configured upstream.
func resolveTarget(ctx context.Context, name, upstream string) ([]latencyRecord, error) {
	var out []latencyRecord

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
		resp, _, err := c.ExchangeContext(ctx, m, upstream)
		if err != nil {
			// AAAA may legitimately not exist; don't fail the whole probe if
			// A returned something useful. We'll evaluate below.
			continue
		}
		if resp.Rcode != dns.RcodeSuccess {
			continue
		}
		for _, rr := range resp.Answer {
			switch v := rr.(type) {
			case *dns.A:
				out = append(out, latencyRecord{
					Target: name,
					Type:   dns.TypeA,
					Value:  v.A.String(),
					TTL:    v.Hdr.Ttl,
				})
			case *dns.AAAA:
				out = append(out, latencyRecord{
					Target: name,
					Type:   dns.TypeAAAA,
					Value:  v.AAAA.String(),
					TTL:    v.Hdr.Ttl,
				})
			case *dns.CNAME:
				out = append(out, latencyRecord{
					Target: name,
					Type:   dns.TypeCNAME,
					Value:  v.Target,
					TTL:    v.Hdr.Ttl,
				})
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no A/AAAA/CNAME answers from %s", upstream)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

func (s *Server) handle(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = false
	m.RecursionAvailable = true

	if len(r.Question) == 0 {
		m.SetRcode(r, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	name := strings.ToLower(q.Name)

	s.mu.RLock()
	z, ok := s.zones[name]
	s.mu.RUnlock()

	if !ok {
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}

	if len(z.Records) == 0 {
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}

	ttl := uint32(60)
	for _, rec := range z.Records {
		if rec.TTL > 0 && rec.TTL < ttl {
			ttl = rec.TTL
		}
	}

	for _, rec := range z.Records {
		switch rec.Type {
		case dns.TypeA:
			if q.Qtype != dns.TypeA {
				continue
			}
			ip := net.ParseIP(rec.Value)
			if ip == nil {
				continue
			}
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
				A:   ip,
			})
		case dns.TypeAAAA:
			if q.Qtype != dns.TypeAAAA {
				continue
			}
			ip := net.ParseIP(rec.Value)
			if ip == nil {
				continue
			}
			m.Answer = append(m.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl},
				AAAA: ip,
			})
		case dns.TypeCNAME:
			m.Answer = append(m.Answer, &dns.CNAME{
				Hdr:    dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl},
				Target: rec.Value,
			})
		}
	}

	if len(m.Answer) == 0 {
		m.SetRcode(r, dns.RcodeNameError)
	} else {
		m.SetRcode(r, dns.RcodeSuccess)
	}
	_ = w.WriteMsg(m)
}

// Healthy reports whether every least-latency zone has been probed at least
// once and has at least one cached record. Used by health checks.
func (s *Server) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, z := range s.zones {
		if z.Updated.IsZero() || len(z.Records) == 0 {
			return false
		}
	}
	return true
}

// ZoneCount returns the number of exact-match zones currently managed by
// the proxy.
func (s *Server) ZoneCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.zones)
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "use of closed network connection")
}
