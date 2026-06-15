package unbound

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// TestResult reports which upstream answered and the response details.
type TestResult struct {
	Question      string        `json:"question"`
	Qtype         string        `json:"qtype"`
	Upstream      string        `json:"upstream"`
	RTT           time.Duration `json:"rtt_ms"`
	Answers       []string      `json:"answers"`
	Authoritative bool          `json:"authoritative"`
	Raw           string        `json:"raw"`
}

func TestResolve(ctx context.Context, name, qtype, upstream string) (*TestResult, error) {
	_ = ctx
	if !strings.Contains(upstream, ":") {
		upstream += ":53"
	}
	m := new(dns.Msg)
	qtypeUint, ok := dns.StringToType[strings.ToUpper(qtype)]
	if !ok {
		return nil, fmt.Errorf("unknown qtype %q", qtype)
	}
	m.SetQuestion(dns.Fqdn(name), qtypeUint)
	c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
	start := time.Now()
	resp, _, err := c.Exchange(m, upstream)
	if err != nil {
		return nil, err
	}
	rtt := time.Since(start)
	answers := make([]string, 0, len(resp.Answer))
	for _, a := range resp.Answer {
		answers = append(answers, a.String())
	}
	return &TestResult{
		Question:      dns.Fqdn(name),
		Qtype:         dns.TypeToString[qtypeUint],
		Upstream:      upstream,
		RTT:           rtt,
		Answers:       answers,
		Authoritative: resp.Authoritative,
		Raw:           resp.String(),
	}, nil
}
