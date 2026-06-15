package proxy

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	icmpTimeout = 2 * time.Second
	icmpPayload = "AzureDNSForwarder"
)

// ping returns the round-trip time to addr using an ICMP echo request. A zero
// duration means the target did not answer (or ICMP is blocked). This uses the
// unprivileged ICMP socket support available on Linux 2.6.30+ and does not
// require raw sockets or CAP_NET_RAW.
func ping(ctx context.Context, addr string) time.Duration {
	ip := net.ParseIP(addr)
	if ip == nil {
		return 0
	}

	var network string
	var proto int
	if ip.To4() != nil {
		network = "ip4:icmp"
		proto = ipv4.ICMPTypeEcho.Protocol()
	} else {
		network = "ip6:ipv6-icmp"
		proto = ipv6.ICMPTypeEchoRequest.Protocol()
	}

	c, err := icmp.ListenPacket(network, "")
	if err != nil {
		return 0
	}
	defer c.Close()

	id := randInt(1 << 16)
	seq := randInt(1 << 16)
	data := append([]byte(icmpPayload), uint16Bytes(id)...)
	data = append(data, uint16Bytes(seq)...)

	var typ icmp.Type
	if ip.To4() != nil {
		typ = ipv4.ICMPTypeEcho
	} else {
		typ = ipv6.ICMPTypeEchoRequest
	}

	m := &icmp.Message{
		Type: typ,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: data,
		},
	}
	wb, err := m.Marshal(nil)
	if err != nil {
		return 0
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(icmpTimeout)
	}
	_ = c.SetDeadline(deadline)

	start := time.Now()
	if _, err := c.WriteTo(wb, &net.IPAddr{IP: ip}); err != nil {
		return 0
	}

	reply := make([]byte, 1500)
	for {
		n, peer, err := c.ReadFrom(reply)
		if err != nil {
			return 0
		}
		if peer.String() != addr {
			continue
		}
		rtt := time.Since(start)

		rm, err := icmp.ParseMessage(proto, reply[:n])
		if err != nil {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if echo.ID != id || echo.Seq != seq {
			continue
		}
		return rtt
	}
}

// concurrentPinger pings many addresses in parallel and returns the best RTT.
// If every ping fails, best is 0 so callers can fall back to returning all
// records unchanged.
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
	if best < 0 {
		best = 0
	}
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

func randInt(max int) int {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return int(time.Now().UnixNano()) % max
	}
	return int(binary.BigEndian.Uint32(b)) % max
}

func uint16Bytes(v int) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(v))
	return b
}

// stringsContains reports whether s contains substr.
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
