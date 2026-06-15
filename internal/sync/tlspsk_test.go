package sync

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/barry/AzureDNSForwarder/internal/config"
)

func TestCertFromPSKDeterministic(t *testing.T) {
	psk := "my-test-preshared-key"

	cert1, err := certFromPSK(psk)
	if err != nil {
		t.Fatalf("certFromPSK: %v", err)
	}
	cert2, err := certFromPSK(psk)
	if err != nil {
		t.Fatalf("certFromPSK second: %v", err)
	}

	if cert1.Leaf == nil || cert2.Leaf == nil {
		t.Fatal("expected parsed leaf certificate")
	}
	if err := publicKeysEqual(cert1.Leaf.PublicKey, cert2.Leaf.PublicKey); err != nil {
		t.Fatalf("public keys differ for same PSK: %v", err)
	}

	certOther, err := certFromPSK("different-key")
	if err != nil {
		t.Fatalf("certFromPSK other: %v", err)
	}
	if err := publicKeysEqual(cert1.Leaf.PublicKey, certOther.Leaf.PublicKey); err == nil {
		t.Fatal("different PSK produced same public key")
	}
}

func TestCertFromPSKEmpty(t *testing.T) {
	_, err := certFromPSK("")
	if err == nil {
		t.Fatal("expected error for empty PSK")
	}
}

func TestTLSConfigForClientPinsKey(t *testing.T) {
	psk := "pin-test-key"
	serverCfg, err := tlsConfigForServer(psk)
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	clientCfg, err := tlsConfigForClient(psk)
	if err != nil {
		t.Fatalf("client config: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 1)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
				_ = conn.Close()
			}()
		}
	}()

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientCfg},
	}

	url := fmt.Sprintf("https://%s/", ln.Addr().String())
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("client get with matching PSK: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestTLSConfigForClientRejectsWrongKey(t *testing.T) {
	serverCfg, err := tlsConfigForServer("server-key")
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	clientCfg, err := tlsConfigForClient("client-key")
	if err != nil {
		t.Fatalf("client config: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientCfg},
	}

	url := fmt.Sprintf("https://%s/", ln.Addr().String())
	_, err = client.Get(url)
	if err == nil {
		t.Fatal("expected TLS error for mismatched PSK public key")
	}
}

func TestListenerStartUsesTLS(t *testing.T) {
	psk := "listener-test-key"
	ln := NewListener(ListenerConfig{
		Listen:   "127.0.0.1:0",
		PSK:      psk,
		Instance: "test-instance",
		Provider: func() *config.File { return nil },
		Logger:   testLogger(t),
	})

	if err := ln.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ln.Stop(context.Background())

	// Plain HTTP should fail at the TLS layer.
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + ln.Addr()
	_, err := client.Get(url)
	if err == nil {
		t.Fatal("plain HTTP to TLS listener should fail")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
