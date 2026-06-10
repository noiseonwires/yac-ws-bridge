package handler

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/bridge-to-freedom/adapter/internal/config"
	"github.com/bridge-to-freedom/adapter/internal/protocol"
	"github.com/gorilla/websocket"
)

func TestCompareSeqUsesYandexReverseChronologicalOrder(t *testing.T) {
	// Lexicographic comparison itself: digits sort before by value ("10" < "2").
	if cmp, ok := compareSeq("10", "2"); !ok || cmp >= 0 {
		t.Fatalf("compareSeq(10, 2) = %d, %v; want alphabetic less-than", cmp, ok)
	}

	// Descending mode (Yandex default): IDs are reverse-chronological, so the
	// LARGEST ID is the OLDEST message and must be delivered first. char-7 here is
	// e > b > 9, so "oldest" (e) sorts first and "newest" (9) sorts last.
	desc := reorderConfig{seqDescending: true}
	msgs := []pendingMsg{
		{seqID: "6engee39ic6nnoe0drji4m5q8cev97", arrival: 1, data: []byte("newest")},
		{seqID: "6engee3b8u67pisdejv1el5cfupqpi", arrival: 2, data: []byte("middle")},
		{seqID: "6engee3ecpt86e54gb27ndjcrv2p3p", arrival: 3, data: []byte("oldest")},
	}
	sort.SliceStable(msgs, func(i, j int) bool { return desc.pendingLess(msgs[i], msgs[j]) })

	got := []string{string(msgs[0].data), string(msgs[1].data), string(msgs[2].data)}
	want := []string{"oldest", "middle", "newest"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("descending sorted order = %v; want %v", got, want)
		}
	}
}

func TestReorderSortDirectionIsConfigurable(t *testing.T) {
	msgs := func() []pendingMsg {
		return []pendingMsg{
			{seqID: "a-large", arrival: 1, data: []byte("A")},
			{seqID: "m-mid", arrival: 2, data: []byte("M")},
			{seqID: "z-small", arrival: 3, data: []byte("Z")},
		}
	}

	// Ascending mode: smallest ID is oldest, so "A" (a-...) sorts first.
	asc := reorderConfig{seqDescending: false}
	a := msgs()
	sort.SliceStable(a, func(i, j int) bool { return asc.pendingLess(a[i], a[j]) })
	if got := []string{string(a[0].data), string(a[1].data), string(a[2].data)}; got[0] != "A" || got[2] != "Z" {
		t.Fatalf("ascending sorted order = %v; want A...Z", got)
	}
	// Ascending: an ID smaller than the last flushed (newest=largest) is late.
	if !asc.isLateSeq("m-mid", "a-large") {
		t.Fatal("ascending: expected a-large to be late after m-mid")
	}
	if asc.isLateSeq("m-mid", "z-small") {
		t.Fatal("ascending: z-small should not be late after m-mid")
	}

	// Descending mode: same input sorts the other way ("Z" smallest = oldest).
	desc := reorderConfig{seqDescending: true}
	d := msgs()
	sort.SliceStable(d, func(i, j int) bool { return desc.pendingLess(d[i], d[j]) })
	if got := []string{string(d[0].data), string(d[1].data), string(d[2].data)}; got[0] != "Z" || got[2] != "A" {
		t.Fatalf("descending sorted order = %v; want Z...A", got)
	}
	// Descending: an ID larger than the last flushed (newest=smallest) is late.
	if !desc.isLateSeq("m-mid", "z-small") {
		t.Fatal("descending: expected z-small to be late after m-mid")
	}
	if desc.isLateSeq("m-mid", "a-large") {
		t.Fatal("descending: a-large should not be late after m-mid")
	}
}

func TestDataC2TReordersAfterTargetConnected(t *testing.T) {
	received := make(chan string, 2)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read target message: %v", err)
				return
			}
			received <- string(data)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Target.URL = "ws" + server.URL[len("http"):]
	cfg.Reorder.C2TDelayMs = 20
	cfg.Reorder.C2TMaxDelayMs = 100
	h := New(cfg)
	defer h.CloseAll()

	clientID := "client-1"
	h.HandleFrame(protocol.Frame{
		ClientID: clientID,
		Type:     protocol.MsgClientConnected,
		Payload:  encodeClientConnectedPayload("/", "", ""),
	})
	waitForTarget(t, h, clientID)

	// Deliver out of order: the NEWEST frame (smallest ID) arrives first, then the
	// OLDER frame (larger ID). The reorder buffer must restore chronological order
	// and write the older frame to the target first.
	h.HandleFrame(protocol.Frame{ClientID: clientID, Type: protocol.MsgDataC2T, SeqID: "6engee39ic6nnoe0drji4m5q8cev97", Payload: []byte("newer")})
	h.HandleFrame(protocol.Frame{ClientID: clientID, Type: protocol.MsgDataC2T, SeqID: "6engee3ecpt86e54gb27ndjcrv2p3p", Payload: []byte("older")})

	first := waitForMessage(t, received)
	second := waitForMessage(t, received)
	if first != "older" || second != "newer" {
		t.Fatalf("target received %q, %q; want %q, %q", first, second, "older", "newer")
	}
}

func waitForTarget(t *testing.T, h *Handler, clientID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		cs := h.clients[clientID]
		connected := cs != nil && cs.targetWS != nil
		h.mu.Unlock()
		if connected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("target websocket was not connected")
}

func waitForMessage(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for target message")
		return ""
	}
}

func encodeClientConnectedPayload(path, subprotocols, iamToken string) []byte {
	pathBytes := []byte(path)
	subBytes := []byte(subprotocols)
	tokenBytes := []byte(iamToken)
	buf := make([]byte, 2+len(pathBytes)+2+len(subBytes)+2+len(tokenBytes))
	off := 0
	putString := func(value []byte) {
		buf[off] = byte(len(value) >> 8)
		buf[off+1] = byte(len(value))
		off += 2
		copy(buf[off:], value)
		off += len(value)
	}
	putString(pathBytes)
	putString(subBytes)
	putString(tokenBytes)
	return buf
}
