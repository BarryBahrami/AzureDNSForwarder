package proxy

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"
)

// ping returns the round-trip time to addr. A zero duration means the
// target did not answer. It uses a TCP SYN to port 53 (DNS), which works
// without raw sockets or CAP_NET_RAW in most containers and matches the
// traffic pattern this app already uses.
func ping(ctx context.Context, addr string) time.Duration {
	ip := net.ParseIP(addr)
	if ip == nil {
		return 0
	}

	rtt, ok := tcpPing(ctx, net.JoinHostPort(addr, "53"))
	if ok {
		return rtt
	}

	// Try the HTTPS port as a fallback for targets that may not run a DNS
	// resolver directly (e.g. CDNs or HTTP load balancers).
	rtt, ok = tcpPing(ctx, net.JoinHostPort(addr, "443"))
	if ok {
		return rtt
	}
	return 0
}

// tcpPing attempts a TCP connection to the supplied host:port and returns
// the handshake RTT on success.
func tcpPing(ctx context.Context, hostport string) (time.Duration, bool) {
	start := time.Now()
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	return time.Since(start), true
}

// concurrentPinger pings many addresses in parallel and returns the best RTT.
func concurrentPinger(ctx context.Context, addrs []string) (map[string]time.Duration, time.Duration) {
	res := make(map[string]time.Duration, len(addrs))
	var mu sync.Mutex
	var best time.Duration = -1

	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			rtt := ping(ctx, addr)
			mu.Lock()
			res[addr] = rtt
			if rtt > 0 && (best < 0 || rtt < best) {
				best = rtt
			}
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	return res, best
}

// parseUpstream splits host:port with a default port of 53.
func parseUpstream(s string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		if stringsContains(s, ":") {
			return "", 0, err
		}
		return s, 53, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func stringsContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > 0 && containsSubstr(s, substr)))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
