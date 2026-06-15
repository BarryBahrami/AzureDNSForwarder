package sync

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const (
	tlsPSKCommonName  = "dnsforwarder-peer"
	tlsPSKOrg         = "dnsforwarder"
	certValidityYears = 10
	certEpoch         = "2024-01-01T00:00:00Z"
)

// certFromPSK derives a self-signed ECDSA certificate and key from a
// preshared key. Peers that share the same PSK generate certificates with
// the same public key, so the client can verify the listener by public
// key without a public CA, certificate files, or persistent state.
//
// The private key is regenerated from HKDF each time; it never needs to be
// stored. The certificate validity, serial number and subject are fixed so
// that every peer that knows the PSK can recognize the same public key.
func certFromPSK(psk string) (tls.Certificate, error) {
	empty := tls.Certificate{}
	if psk == "" {
		return empty, fmt.Errorf("psk is empty")
	}

	// Derive a 256-bit seed from the UTF-8 PSK using HKDF-SHA256 with a
	// fixed salt. The salt is part of the protocol version; changing it
	// would break interoperability between peers.
	salt := []byte("dnsforwarder-peer-sync-v1")
	hk := hkdf.New(sha256.New, []byte(psk), salt, nil)
	seed := make([]byte, 32)
	if _, err := hk.Read(seed); err != nil {
		return empty, fmt.Errorf("hkdf: %w", err)
	}

	// Deterministic ECDSA P-256 key from the seed.
	priv, err := ecdsaKeyFromSeed(seed)
	if err != nil {
		return empty, fmt.Errorf("derive key: %w", err)
	}

	// Deterministic serial = first 128 bits of SHA256(seed).
	serial := new(big.Int).SetBytes(seedSha(seed)[:16])

	notBefore, err := time.Parse(time.RFC3339, certEpoch)
	if err != nil {
		return empty, fmt.Errorf("epoch: %w", err)
	}
	notAfter := notBefore.Add(certValidityYears * 365 * 24 * time.Hour)

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{tlsPSKOrg},
			CommonName:   tlsPSKCommonName,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return empty, fmt.Errorf("create cert: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return empty, fmt.Errorf("parse cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return empty, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	loaded, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return empty, fmt.Errorf("load pair: %w", err)
	}
	loaded.Leaf = cert
	return loaded, nil
}

// tlsConfigForServer returns a tls.Config for the peer listener using the
// certificate derived from the PSK.
func tlsConfigForServer(psk string) (*tls.Config, error) {
	cert, err := certFromPSK(psk)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// tlsConfigForClient returns a tls.Config for peer clients. It pins the
// listener's public key to the ECDSA public key derived from the same PSK,
// so only a peer that knows the PSK can complete the TLS handshake.
func tlsConfigForClient(psk string) (*tls.Config, error) {
	cert, err := certFromPSK(psk)
	if err != nil {
		return nil, err
	}
	expected := cert.Leaf.PublicKey
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            x509.NewCertPool(),
		InsecureSkipVerify: true, // we verify the peer public key ourselves below
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			return verifyPeerPublicKey(rawCerts, expected)
		},
	}, nil
}

// verifyPeerPublicKey checks that the first presented certificate carries the
// expected PSK-derived public key.
func verifyPeerPublicKey(rawCerts [][]byte, expected any) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("peer presented no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("parse peer cert: %w", err)
	}
	if err := publicKeysEqual(cert.PublicKey, expected); err != nil {
		return fmt.Errorf("peer public key mismatch: %w", err)
	}
	return nil
}

// publicKeysEqual compares two public keys. Only ECDSA P-256 is supported
// because that is what certFromPSK generates.
func publicKeysEqual(a, b any) error {
	ak, ok := a.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("unsupported peer public key type %T", a)
	}
	bk, ok := b.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("expected ECDSA public key, got %T", b)
	}
	if ak.Curve != bk.Curve {
		return fmt.Errorf("curve mismatch")
	}
	if ak.X.Cmp(bk.X) != 0 || ak.Y.Cmp(bk.Y) != 0 {
		return fmt.Errorf("key coordinates differ")
	}
	return nil
}

// publicKeyFingerprint returns the SHA-256 digest of the uncompressed ECDSA
// public key. It is used for logging, not for cryptographic verification.
func publicKeyFingerprint(pub *ecdsa.PublicKey) []byte {
	sum := sha256.Sum256(elliptic.Marshal(pub.Curve, pub.X, pub.Y))
	return sum[:]
}

// fingerprintString formats a fingerprint the same way OpenSSL does.
func fingerprintString(fp []byte) string {
	var sb strings.Builder
	for i, b := range fp {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(fmt.Sprintf("%02X", b))
	}
	return sb.String()
}

// fingerprintBase64 returns the URL-safe base64 fingerprint for compact logs.
func fingerprintBase64(fp []byte) string {
	return base64.RawURLEncoding.EncodeToString(fp)
}

// seedSha returns SHA256(seed) for deterministic serial derivation.
func seedSha(seed []byte) []byte {
	sum := sha256.Sum256(seed)
	return sum[:]
}

// ecdsaKeyFromSeed deterministically creates an ECDSA P-256 private key from a
// 32-byte seed. The seed is interpreted as a scalar and reduced modulo the
// curve order until a valid private key is produced. This is deterministic
// across all peers and never produces weak or all-zero keys.
func ecdsaKeyFromSeed(seed []byte) (*ecdsa.PrivateKey, error) {
	if len(seed) != 32 {
		return nil, fmt.Errorf("seed must be 32 bytes, got %d", len(seed))
	}
	curve := elliptic.P256()
	order := curve.Params().N

	d := new(big.Int).SetBytes(seed)
	for i := 0; i < 10000; i++ {
		if d.Sign() > 0 && d.Cmp(order) < 0 {
			priv := new(ecdsa.PrivateKey)
			priv.PublicKey.Curve = curve
			priv.D = d
			priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(d.Bytes())
			if priv.PublicKey.X != nil {
				return priv, nil
			}
		}
		// Deterministically re-seed with a counter and re-derive.
		h := sha256.New()
		_, _ = h.Write(seed)
		_, _ = h.Write([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
		d.SetBytes(h.Sum(nil))
	}
	return nil, fmt.Errorf("could not derive a valid ECDSA key from seed")
}
