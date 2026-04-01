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
// It closes both sides when either direction finishes.
func Relay(qs quic.Stream, tc net.Conn) {
	if t, ok := tc.(*net.TCPConn); ok {
		t.SetNoDelay(true)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// QUIC → TCP
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufSize)
		if _, err := io.CopyBuffer(tc, qs, buf); err != nil {
			log.Printf("[DEBUG] quic→tcp err stream=%d: %v", qs.StreamID(), err)
		}
		if t, ok := tc.(*net.TCPConn); ok {
			t.CloseWrite()
		}
	}()

	// TCP → QUIC
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufSize)
		if _, err := io.CopyBuffer(qs, tc, buf); err != nil {
			log.Printf("[DEBUG] tcp→quic err stream=%d: %v", qs.StreamID(), err)
		}
		qs.Close()
	}()

	wg.Wait()
	tc.Close()
}

