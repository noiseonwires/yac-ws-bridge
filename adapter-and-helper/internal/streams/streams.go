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
		if _, err := io.Copy(tc, qs); err != nil {
			log.Printf("[DEBUG] quic→tcp err stream=%d: %v", qs.StreamID(), err)
		}
		// Signal TCP that no more data is coming.
		if t, ok := tc.(*net.TCPConn); ok {
			t.CloseWrite()
		}
	}()

	// TCP → QUIC
	go func() {
		defer wg.Done()
		if _, err := io.Copy(qs, tc); err != nil {
			log.Printf("[DEBUG] tcp→quic err stream=%d: %v", qs.StreamID(), err)
		}
		// Signal QUIC that no more data is coming.
		qs.Close()
	}()

	wg.Wait()
	tc.Close()
}

