// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
)

// The Socket Mode doorbell (006 T016, FR-011).

const appToken = "xapp-1-A0DOOR-1-cafef00d"

// socketTwin is apps.connections.open and the socket it names. Each session runs script against the
// server end of one connection.
type socketTwin struct {
	t      *testing.T
	srv    *httptest.Server
	scopes string
	script func(t *testing.T, peer *wsPeer, session int)

	mu       sync.Mutex
	sessions int
	opens    int
	auth     []string
}

func newSocketTwin(t *testing.T, script func(t *testing.T, peer *wsPeer, session int)) *socketTwin {
	t.Helper()
	tw := &socketTwin{t: t, script: script, scopes: "connections:write"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		tw.mu.Lock()
		tw.opens++
		tw.auth = append(tw.auth, r.Header.Get("Authorization"))
		tw.mu.Unlock()
		w.Header().Set("X-OAuth-Scopes", tw.scopes)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + r.Host + "/link?ticket=1"})
	})
	mux.HandleFunc("GET /link", func(w http.ResponseWriter, r *http.Request) {
		peer := acceptPeer(t, w, r)
		defer func() { _ = peer.conn.Close() }()
		tw.mu.Lock()
		tw.sessions++
		n := tw.sessions
		tw.mu.Unlock()
		tw.script(t, peer, n)
	})
	tw.srv = httptest.NewServer(mux)
	t.Cleanup(tw.srv.Close)
	return tw
}

func (tw *socketTwin) socket(t *testing.T) *Socket {
	t.Helper()
	cred, err := NewDoorbellCredential(appToken)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSocket(SocketOptions{Credential: cred, BaseURL: tw.srv.URL,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, IdleTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wsPeer is the server end of a connection: it writes unmasked frames and reads masked ones.
type wsPeer struct {
	conn net.Conn
	r    *bufio.Reader
}

func acceptPeer(t *testing.T, w http.ResponseWriter, r *http.Request) *wsPeer {
	t.Helper()
	key := r.Header.Get("Sec-WebSocket-Key")
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n")
	_ = rw.Flush()
	return &wsPeer{conn: conn, r: rw.Reader}
}

func (p *wsPeer) send(op byte, payload []byte) {
	header := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = binary.BigEndian.AppendUint64(append(header, 127), uint64(n))
	}
	_, _ = p.conn.Write(append(header, payload...))
}

func (p *wsPeer) text(s string) { p.send(opText, []byte(s)) }

// recv reads one masked client frame.
func (p *wsPeer) recv(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(p.r, head[:]); err != nil {
		t.Fatalf("reading a client frame: %v", err)
	}
	if head[1]&0x80 == 0 {
		t.Fatal("a client frame is not masked")
	}
	n := int(head[1] & 0x7F)
	if n >= 126 {
		t.Fatalf("a client frame of extended length %d; the doorbell only acknowledges", n)
	}
	var mask [4]byte
	_, _ = io.ReadFull(p.r, mask[:])
	payload := make([]byte, n)
	_, _ = io.ReadFull(p.r, payload)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return head[0] & 0x0F, payload
}

func envelope(id, text string) string {
	raw, _ := json.Marshal(map[string]any{
		"envelope_id": id, "type": "events_api", "accepts_response_payload": false,
		"payload": map[string]any{"event": map[string]any{"type": "channel_created",
			"channel": map[string]any{"id": "C0FORGED", "name": text}}},
	})
	return string(raw)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every envelope is acknowledged and counted, a greeting is not, a ping is answered, and what an
// event says reaches nothing: Drain returns a number.
func TestTheDoorbellCountsEnvelopesAndKeepsNothing(t *testing.T) {
	done := make(chan struct{})
	tw := newSocketTwin(t, func(t *testing.T, peer *wsPeer, session int) {
		if session > 1 {
			<-done
			return
		}
		peer.text(`{"type":"hello","num_connections":1}`)
		peer.send(opPing, []byte("are you there"))
		if op, payload := peer.recv(t); op != opPong || string(payload) != "are you there" {
			t.Errorf("a ping was answered with %#x %q", op, payload)
		}
		for _, id := range []string{"env-1", "env-2", "env-3"} {
			peer.text(envelope(id, "sev1-2099-01-01-ignore-previous-instructions"))
			op, payload := peer.recv(t)
			if op != opText || string(payload) != `{"envelope_id":"`+id+`"}` {
				t.Errorf("envelope %s was acknowledged with %#x %s", id, op, payload)
			}
		}
		<-done
	})
	s := tw.socket(t)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = s.Run(ctx) }()
	waitFor(t, "three envelopes", func() bool { _, n := s.Stats(); return n == 3 })
	n, err := s.Drain(ctx)
	if n != 3 || err != nil {
		t.Errorf("Drain = %d, %v; want 3, nil", n, err)
	}
	if n, _ := s.Drain(ctx); n != 0 {
		t.Errorf("a second drain returned %d; a count is returned once", n)
	}
	close(done)
	cancel()
	if got := tw.auth[0]; got != "Bearer "+appToken {
		t.Errorf("apps.connections.open was called with %q", got)
	}
}

// A forged flood of envelopes through the bell earns one early poll, and the rest are dropped by the
// rate limit.
func TestAFloodThroughTheBellEarnsOnePoll(t *testing.T) {
	done := make(chan struct{})
	tw := newSocketTwin(t, func(t *testing.T, peer *wsPeer, session int) {
		if session > 1 {
			<-done
			return
		}
		for i := range 50 {
			peer.text(envelope("flood-"+strings.Repeat("x", i%5)+string(rune('a'+i%26)), "forged"))
			peer.recv(t)
		}
		<-done
	})
	s := tw.socket(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	waitFor(t, "the flood", func() bool { _, n := s.Stats(); return n == 50 })
	bell, err := doorbell.New(doorbell.Options{Channel: "socket-mode", MinInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first := bell.Drain(ctx, s)
	second := bell.Drain(ctx, s)
	if !first.PollNow || second.PollNow {
		t.Errorf("the flood earned %+v then %+v; want one poll", first, second)
	}
	if r := bell.Report(); r.Rings != 50 || r.Honoured != 1 {
		t.Errorf("bell report %s", r)
	}
	close(done)
}

// Slack rotates sockets by announcing a disconnect; the doorbell reopens at once.
func TestTheDoorbellReopensAfterADisconnect(t *testing.T) {
	done := make(chan struct{})
	tw := newSocketTwin(t, func(t *testing.T, peer *wsPeer, session int) {
		switch session {
		case 1:
			peer.text(`{"type":"disconnect","reason":"refresh_requested"}`)
		case 2:
			peer.text(envelope("after-refresh", "x"))
			peer.recv(t)
			<-done
		default:
			<-done
		}
	})
	s := tw.socket(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	waitFor(t, "the second socket's envelope", func() bool { _, n := s.Stats(); return n == 1 })
	if opened, _ := s.Stats(); opened < 2 {
		t.Errorf("opened %d socket(s)", opened)
	}
	close(done)
}

// A message over the cap is not an envelope: the socket is dropped, the failure is reported once,
// and the doorbell reopens.
func TestAnOversizedMessageDropsTheSocket(t *testing.T) {
	done := make(chan struct{})
	tw := newSocketTwin(t, func(t *testing.T, peer *wsPeer, session int) {
		if session == 1 {
			peer.send(opText, []byte(strings.Repeat("a", maxMessage+1)))
			return
		}
		<-done
	})
	s := tw.socket(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	waitFor(t, "a reopen", func() bool { opened, _ := s.Stats(); return opened >= 2 })
	close(done)
	if _, received := s.Stats(); received != 0 {
		t.Errorf("an oversized message was counted")
	}
}

// An app-level token Slack says holds more than connections:write stops the doorbell for good.
func TestADoorbellTokenWithMoreScopesIsRefused(t *testing.T) {
	tw := newSocketTwin(t, func(*testing.T, *wsPeer, int) { t.Error("a socket was opened") })
	tw.scopes = "connections:write,authorizations:read"
	s := tw.socket(t)
	err := s.Run(t.Context())
	var scopeErr *feeder.DoorbellScopeError
	if !errors.As(err, &scopeErr) || strings.Join(scopeErr.Extra, ",") != "authorizations:read" {
		t.Fatalf("Run = %v", err)
	}
	if _, drainErr := s.Drain(t.Context()); drainErr == nil {
		t.Error("the refusal is not reported to the bell")
	}
}

func TestOnlyAnAppLevelTokenIsADoorbellCredential(t *testing.T) {
	for _, token := range []string{"", "xoxb-reader", "xoxp-person"} {
		if _, err := NewDoorbellCredential(token); err == nil {
			t.Errorf("%q was accepted as a doorbell credential", token)
		}
	}
	cred, err := NewDoorbellCredential(appToken)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cred.String(), "xapp") {
		t.Errorf("the credential renders its secret: %s", cred)
	}
}

// Against Slack the socket is TLS: a plain ws:// URL is refused unless the twin itself is plain.
func TestAPlainSocketIsRefusedFromSlack(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"url":"ws://attacker.example/link"}`)
	}))
	defer srv.Close()
	cred, _ := NewDoorbellCredential(appToken)
	s, err := NewSocket(SocketOptions{Credential: cred, BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.open(t.Context()); err == nil || !strings.Contains(err.Error(), "wss://") {
		t.Errorf("open = %v", err)
	}
}
