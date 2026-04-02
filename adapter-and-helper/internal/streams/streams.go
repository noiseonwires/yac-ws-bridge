// Package streams provides helpers for bidirectional copying between
// a QUIC stream and a TCP connection.
package streams

import (
	"io"
	"log"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

// copyBufSize is the buffer size for relay copies.
// Large buffer reduces syscall overhead and lets QUIC send larger frames.
const copyBufSize = 256 * 1024 // 256 KiB

// Relay copies data bidirectionally between a QUIC stream and a TCP connection.
// When either direction finishes or errors, the other direction is torn down
// promptly to avoid hanging goroutines and stalled browser resources.
func Relay(qs quic.Stream, tc net.Conn) {
	if t, ok := tc.(*net.TCPConn); ok {
		t.SetNoDelay(true)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// teardown closes both sides so the other goroutine unblocks.
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			// CancelRead makes the QUIC→TCP reader return immediately.
			qs.CancelRead(0)
			// Close TCP so TCP→QUIC writer returns.
			tc.Close()
		})
	}

	// QUIC → TCP
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufSize)
		_, err := io.CopyBuffer(tc, qs, buf)
		if err != nil {
			log.Printf("[DEBUG] quic→tcp err stream=%d: %v", qs.StreamID(), err)
		}
		// Graceful half-close if possible.
		if t, ok := tc.(*net.TCPConn); ok {
			t.CloseWrite()
		}
		teardown()
	}()

	// TCP → QUIC
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufSize)
		_, err := io.CopyBuffer(qs, tc, buf)
		if err != nil {
			log.Printf("[DEBUG] tcp→quic err stream=%d: %v", qs.StreamID(), err)
		}
		qs.Close()
		teardown()
	}()

	wg.Wait()
	tc.Close()
}

