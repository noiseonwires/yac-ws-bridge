// Package tun owns the local TUN device and pumps IP packets between it and
// a peer-bound WebSocket transport. It deliberately stays unaware of TCP
// streams: the OS network stack on each endpoint handles reordering,
// retransmission, and flow control, and this layer is just a packet conduit.
package tun

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/protocol"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// SendFunc transmits a single encoded protocol frame to the peer.
type SendFunc func(data []byte) error

// virtioNetHdrLen is the size of the virtio-net header that wireguard-go's
// Linux TUN driver expects to live in the headroom before each packet when
// GRO/GSO is enabled. Read/Write require offset >= virtioNetHdrLen and the
// buffer to have at least that much space before the packet starts; passing
// 0 triggers "invalid offset" from handleGRO. Other platforms ignore the
// offset, so using the same value everywhere is safe.
const virtioNetHdrLen = 10

// Config describes the local TUN device.
type Config struct {
	// Name is the requested device name. On Windows this becomes the wintun
	// adapter name; on Linux the kernel iface name; on macOS this is ignored
	// (utun assigns a name).
	Name string
	// Address is the local tunnel IP (e.g. 10.200.0.2 on the helper).
	Address netip.Addr
	// PeerAddress is the remote tunnel IP (e.g. 10.200.0.1 for the adapter).
	// On Windows it is required to compute the wintun /30 routing.
	PeerAddress netip.Addr
	// MTU for the device. 1280 is a safe default; 1400 is reasonable for
	// wss tunnels through Yandex (the WebSocket frame plus TLS plus the
	// outer IP header eats roughly 60-100 bytes).
	MTU int
	// CoalesceDelay, when > 0, batches IP packets read from the TUN within
	// this window into a single MsgPacketBatch frame.
	CoalesceDelay time.Duration
	// CoalesceMaxBytes caps the batched payload size before forced flush.
	// 32 KiB matches the previous TCP-stream behaviour.
	CoalesceMaxBytes int
}

// Device wraps an open TUN and exposes packet-pump primitives.
type Device struct {
	cfg  Config
	dev  wgtun.Device
	name string

	send     SendFunc
	sendMu   sync.Mutex
	sendable atomic.Bool

	// stats for logs
	rxPkts atomic.Uint64
	txPkts atomic.Uint64

	// one-shot first-error diagnostics
	writeErrLogged atomic.Bool
}

// Open creates and configures the TUN interface.
func Open(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = 1400
	}
	if cfg.CoalesceMaxBytes <= 0 {
		cfg.CoalesceMaxBytes = 32 * 1024
	}
	if !cfg.Address.IsValid() {
		return nil, errors.New("tun: Address is required")
	}

	dev, err := wgtun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("read TUN name: %w", err)
	}
	d := &Device{cfg: cfg, dev: dev, name: name}
	if err := d.configureInterface(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure %s: %w", name, err)
	}
	log.Printf("[INFO] TUN opened name=%s address=%s peer=%s mtu=%d", name, cfg.Address, cfg.PeerAddress, cfg.MTU)
	return d, nil
}

// Name returns the OS interface name.
func (d *Device) Name() string { return d.name }

// SetSend installs the function used to ship outbound packets to the peer.
// May be called repeatedly (e.g. on upstream reconnect).
func (d *Device) SetSend(fn SendFunc) {
	d.sendMu.Lock()
	d.send = fn
	d.sendMu.Unlock()
	d.sendable.Store(fn != nil)
}

// WritePacket writes one IP packet from the peer to the TUN.
func (d *Device) WritePacket(pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	// Allocate with virtioNetHdrLen headroom so wireguard-go's Linux GRO path
	// can place the virtio-net header before the packet bytes.
	buf := make([]byte, virtioNetHdrLen+len(pkt))
	copy(buf[virtioNetHdrLen:], pkt)
	bufs := [][]byte{buf}
	_, err := d.dev.Write(bufs, virtioNetHdrLen)
	if err != nil && d.writeErrLogged.CompareAndSwap(false, true) {
		log.Printf("[DEBUG] tun write FIRST FAIL: err=%v pktLen=%d bufLen=%d offset=%d virtioHdrLen=%d firstByte=0x%02x",
			err, len(pkt), len(buf), virtioNetHdrLen, virtioNetHdrLen, buf[virtioNetHdrLen])
	}
	if err == nil {
		d.txPkts.Add(1)
	}
	return err
}

// ReadLoop reads IP packets from the TUN and ships them via SendFunc.
// Returns when ctx is done or the device errors.
func (d *Device) ReadLoop(ctx context.Context) {
	mtu := d.cfg.MTU
	// Allocate per-iteration so the buffer can be safely retained by the batch.
	// Reserve virtioNetHdrLen of headroom before the packet for the Linux GRO
	// path; the IP packet itself will land at bufs[0][virtioNetHdrLen:].
	bufs := [][]byte{make([]byte, virtioNetHdrLen+mtu+128)}
	sizes := []int{0}

	coalesce := d.cfg.CoalesceDelay > 0
	var (
		batch     [][]byte
		batchSize int
		flushTime time.Time
	)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		var frame protocol.Frame
		if len(batch) == 1 {
			frame = protocol.Frame{Type: protocol.MsgPacket, Payload: batch[0]}
		} else {
			frame = protocol.Frame{Type: protocol.MsgPacketBatch, Payload: protocol.EncodePacketBatch(batch)}
		}
		batch = nil
		batchSize = 0
		d.sendMu.Lock()
		fn := d.send
		d.sendMu.Unlock()
		if fn == nil {
			return // peer not ready, drop (TCP will retransmit)
		}
		if err := fn(protocol.Encode(frame)); err != nil {
			log.Printf("[WARN] tun send failed: %v", err)
		}
	}

	for {
		if ctx.Err() != nil {
			flush()
			return
		}

		// Note: wireguard/tun does not expose a context-aware Read. We rely on
		// Close() (called from Stop) to wake this loop with an error.
		bufs[0] = bufs[0][:cap(bufs[0])]
		n, err := d.dev.Read(bufs, sizes, virtioNetHdrLen)
		if err != nil {
			if ctx.Err() != nil || isClosed(err) {
				flush()
				return
			}
			log.Printf("[WARN] tun read: %v", err)
			flush()
			return
		}
		if n == 0 {
			continue
		}
		size := sizes[0]
		if size <= 0 {
			continue
		}
		// Copy out so we can keep the source buffer for the next read.
		// Read places the packet at bufs[0][virtioNetHdrLen : virtioNetHdrLen+size].
		pkt := make([]byte, size)
		copy(pkt, bufs[0][virtioNetHdrLen:virtioNetHdrLen+size])
		d.rxPkts.Add(1)

		if !coalesce {
			d.sendMu.Lock()
			fn := d.send
			d.sendMu.Unlock()
			if fn == nil {
				continue
			}
			if err := fn(protocol.Encode(protocol.Frame{Type: protocol.MsgPacket, Payload: pkt})); err != nil {
				log.Printf("[WARN] tun send failed: %v", err)
			}
			continue
		}

		// Coalescing path
		if len(batch) == 0 {
			flushTime = time.Now().Add(d.cfg.CoalesceDelay)
		}
		batch = append(batch, pkt)
		batchSize += 2 + len(pkt)
		if batchSize >= d.cfg.CoalesceMaxBytes || !time.Now().Before(flushTime) {
			flush()
		}
	}
}

// Close releases the TUN device.
func (d *Device) Close() error {
	return d.dev.Close()
}

// Stats returns (rxPackets, txPackets). rx = TUN -> peer, tx = peer -> TUN.
func (d *Device) Stats() (rx, tx uint64) {
	return d.rxPkts.Load(), d.txPkts.Load()
}

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return s == "device closed" || s == "file already closed" ||
		errors.Is(err, errClosed)
}

var errClosed = errors.New("closed")

// configureInterface sets the IP address and brings the device up.
// Implementation differs per OS — wireguard/tun creates the device but does
// not assign an address.
func (d *Device) configureInterface() error {
	switch runtime.GOOS {
	case "linux":
		return d.configureLinux()
	case "darwin":
		return d.configureDarwin()
	case "windows":
		return d.configureWindows()
	default:
		log.Printf("[WARN] tun: no automatic interface configuration for GOOS=%s; configure %s manually", runtime.GOOS, d.name)
		return nil
	}
}

func (d *Device) configureLinux() error {
	is6 := d.cfg.Address.Is6()
	var addr string
	if is6 {
		addr = d.cfg.Address.String() + "/64"
	} else {
		addr = d.cfg.Address.String() + "/30"
	}
	if err := runCmd("ip", "addr", "add", addr, "dev", d.name); err != nil {
		return err
	}
	if err := runCmd("ip", "link", "set", "dev", d.name, "mtu", fmt.Sprint(d.cfg.MTU)); err != nil {
		return err
	}
	return runCmd("ip", "link", "set", "dev", d.name, "up")
}

func (d *Device) configureDarwin() error {
	// macOS utun: ifconfig <name> <local> <peer> mtu <mtu> up
	if !d.cfg.PeerAddress.IsValid() {
		return errors.New("darwin: PeerAddress is required")
	}
	return runCmd("ifconfig", d.name, d.cfg.Address.String(), d.cfg.PeerAddress.String(),
		"mtu", fmt.Sprint(d.cfg.MTU), "up")
}

func (d *Device) configureWindows() error {
	// Use netsh to assign the address. wintun needs the helper netsh dance.
	// This sets the address without a default route — routing is left to the
	// operator (we are not a full VPN, just a point-to-point bridge).
	if d.cfg.Address.Is4() {
		mask := "255.255.255.252" // /30
		return runCmd("netsh", "interface", "ip", "set", "address",
			fmt.Sprintf("name=%s", d.name), "static",
			d.cfg.Address.String(), mask)
	}
	return runCmd("netsh", "interface", "ipv6", "set", "address",
		fmt.Sprintf("interface=%s", d.name), d.cfg.Address.String())
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (%s)", name, args, err, string(out))
	}
	return nil
}

// QuickIPVersion returns 4 or 6 for a likely-valid IP packet, or 0.
func QuickIPVersion(pkt []byte) byte {
	if len(pkt) < 1 {
		return 0
	}
	v := pkt[0] >> 4
	if v == 4 || v == 6 {
		return v
	}
	return 0
}

// Sanity helper to avoid an unused import warning in some build configs.
var _ = binary.BigEndian
