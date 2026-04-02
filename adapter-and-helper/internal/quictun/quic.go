package quictun

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"time"

	"github.com/quic-go/quic-go"
)

// DefaultMTU is the default maximum QUIC packet size.
const DefaultMTU = 1200

// quicConfig returns tuned QUIC settings for high-latency virtual transport.
//
// Key tuning vs defaults:
//   - InitialPacketSize = MTU: skip PMTUD, we know exact path capacity
//   - DisablePathMTUDiscovery: no real UDP path, PMTUD wastes round-trips
//   - InitialStreamReceiveWindow: 8MB — large window for high BDP
//   - InitialConnectionReceiveWindow: 16MB — connection-level flow control
//   - MaxIdleTimeout: 120s — tolerant of WS reconnections
//   - KeepAlivePeriod: 20s — below typical WS idle timeout (60s)
//   - Allow0RTT on server: skips 1-RTT handshake on reconnect
func quicConfig(mtu int) *quic.Config {
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	return &quic.Config{
		// Very long idle timeout since we don't use QUIC keepalive.
		MaxIdleTimeout: 600 * time.Second,
		// Explicit handshake timeout — don't inherit the 600s idle timeout.
		HandshakeIdleTimeout: 30 * time.Second,
		// No QUIC-level keepalive — we use WS-level PING/PONG for liveness.
		KeepAlivePeriod: 0,

		InitialPacketSize:       uint16(mtu),
		DisablePathMTUDiscovery: true,

		// Large flow control windows — the virtual transport has no real
		// bandwidth limit; the bottleneck is wsSend call rate. Large windows
		// let QUIC push data without waiting for window updates.
		InitialStreamReceiveWindow:     8 << 20, // 8 MiB per stream
		MaxStreamReceiveWindow:         16 << 20, // 16 MiB per stream
		InitialConnectionReceiveWindow: 16 << 20, // 16 MiB total
		MaxConnectionReceiveWindow:     32 << 20, // 32 MiB total

		MaxIncomingStreams:    1 << 20,
		MaxIncomingUniStreams: 1 << 20,

		// Allow 0-RTT for faster stream opens on reconnection.
		Allow0RTT: true,
	}
}

// ServerConfig returns TLS + QUIC configs for the adapter (QUIC server) side.
func ServerConfig(mtu int) (*tls.Config, *quic.Config) {
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{selfSignedCert()},
		NextProtos:   []string{"btf-quic"},
	}
	return tlsCfg, quicConfig(mtu)
}

// ClientConfig returns TLS + QUIC configs for the helper (QUIC client) side.
func ClientConfig(mtu int) (*tls.Config, *quic.Config) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // Self-signed cert, verified by auth token.
		NextProtos:         []string{"btf-quic"},
	}
	return tlsCfg, quicConfig(mtu)
}

// ListenQUIC creates a QUIC listener on top of the virtual transport.
// The quic.Transport is created once and stored on the Transport for reuse.
func ListenQUIC(ctx context.Context, transport *Transport, mtu int) (*quic.Listener, error) {
	tlsCfg, quicCfg := ServerConfig(mtu)
	tr := transport.getOrCreateQUICTransport()
	return tr.Listen(tlsCfg, quicCfg)
}

// DialQUIC establishes a QUIC connection to the adapter over the virtual transport.
// Reuses the same quic.Transport across calls to avoid the "connection already exists" panic.
func DialQUIC(ctx context.Context, transport *Transport, mtu int) (quic.Connection, error) {
	tlsCfg, quicCfg := ClientConfig(mtu)
	tr := transport.getOrCreateQUICTransport()
	return tr.Dial(ctx, transport.PeerAddr(), tlsCfg, quicCfg)
}

func selfSignedCert() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return cert
}
