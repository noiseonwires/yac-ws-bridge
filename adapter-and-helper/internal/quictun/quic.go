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

// ServerConfig returns a quic.Transport + quic.Listener for the adapter side.
// The adapter acts as the QUIC server: the helper dials into it.
func ServerConfig(mtu int) (*tls.Config, *quic.Config) {
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{selfSignedCert()},
		NextProtos:   []string{"btf-quic"},
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 60 * time.Second,
		KeepAlivePeriod:                15 * time.Second,
		InitialPacketSize:              uint16(mtu),
		DisablePathMTUDiscovery:        true,
		MaxIncomingStreams:             1 << 20,
		MaxIncomingUniStreams:          1 << 20,
	}
	return tlsCfg, quicCfg
}

// ClientConfig returns TLS + QUIC configs for the helper side.
func ClientConfig(mtu int) (*tls.Config, *quic.Config) {
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // Self-signed cert, verified by auth token.
		NextProtos:         []string{"btf-quic"},
	}
	quicCfg := &quic.Config{
		MaxIdleTimeout:                 60 * time.Second,
		KeepAlivePeriod:                15 * time.Second,
		InitialPacketSize:              uint16(mtu),
		DisablePathMTUDiscovery:        true,
		MaxIncomingStreams:             1 << 20,
		MaxIncomingUniStreams:          1 << 20,
	}
	return tlsCfg, quicCfg
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
