// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 fixes SHA-1 for the handshake's accept key; it secures nothing
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A WebSocket client, as small as Socket Mode needs (research R1).
//
// The doorbell receives text frames, answers pings and acknowledges envelopes; it never sends
// anything else. A general-purpose library would bring a surface this connector does not use, and
// the two that are maintained are licensed outside what the constitution lists, so the client is
// hand-written over net and crypto/tls, as the Web API client is over net/http. What it does not do
// is deliberate: no extensions (so no compression, and no decompression bomb), no subprotocols, and
// a hard cap on a message's size.

// wsGUID is RFC 6455 §1.3's fixed suffix for the accept key.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxMessage bounds one message. A Socket Mode envelope is a few kilobytes; anything near this is
// not one, and reading it would only hold more of somebody's text in memory.
const maxMessage = 1 << 20

// errMessageTooLarge is a message over maxMessage.
var errMessageTooLarge = errors.New("slackx: a socket message over 1 MiB; it is not a Socket Mode envelope")

// wsConn is one client connection.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialWebSocket opens a client connection to a ws:// or wss:// URL.
func dialWebSocket(ctx context.Context, rawURL string, dialer *net.Dialer, tlsConfig *tls.Config) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("slackx: socket url: %w", err)
	}
	host := u.Host
	secure := false
	switch u.Scheme {
	case "wss":
		secure = true
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
	case "ws":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	default:
		return nil, fmt.Errorf("slackx: socket url scheme %q is not ws or wss", u.Scheme)
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 30 * time.Second}
	}
	raw, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("slackx: socket dial: %w", err)
	}
	conn := raw
	if secure {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsConfig != nil {
			cfg = tlsConfig.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("slackx: socket tls: %w", err)
		}
		conn = tlsConn
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	ws, err := handshake(conn, u)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ws, nil
}

func handshake(conn net.Conn, u *url.URL) (*wsConn, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("slackx: socket handshake: %w", err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("slackx: socket handshake: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("slackx: socket handshake answered %d, not 101", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("slackx: socket handshake did not upgrade to websocket")
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		return nil, errors.New("slackx: socket handshake's accept key does not answer ours")
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		return nil, fmt.Errorf("slackx: socket handshake negotiated extensions %q we did not offer", ext)
	}
	return &wsConn{conn: conn, r: r}, nil
}

// acceptKey is RFC 6455 §4.2.2's Sec-WebSocket-Accept for a key.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // fixed by RFC 6455
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readMessage returns the next text or binary message, answering pings on the way. A close frame
// is answered and returns io.EOF. deadline bounds the wait for the next frame.
func (c *wsConn) readMessage(deadline time.Time) ([]byte, error) {
	var message []byte
	inMessage := false
	for {
		_ = c.conn.SetReadDeadline(deadline)
		fin, op, payload, err := c.readFrame(maxMessage - len(message))
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return nil, io.EOF
		case opText, opBinary:
			if inMessage {
				return nil, errors.New("slackx: socket started a message inside another")
			}
			inMessage = true
			message = append(message, payload...)
		case opContinuation:
			if !inMessage {
				return nil, errors.New("slackx: socket continued a message it never started")
			}
			message = append(message, payload...)
		default:
			return nil, fmt.Errorf("slackx: socket sent opcode %#x", op)
		}
		if fin {
			return message, nil
		}
	}
}

// readFrame reads one frame of at most limit payload bytes. A server's frames are unmasked.
func (c *wsConn) readFrame(limit int) (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.r, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("slackx: socket frame sets reserved bits; no extension was negotiated")
	}
	op = head[0] & 0x0F
	if head[1]&0x80 != 0 {
		return false, 0, nil, errors.New("slackx: socket frame from the server is masked")
	}
	n := uint64(head[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if op >= opClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("slackx: socket control frame is fragmented or over 125 bytes")
	}
	if limit < 0 || n > uint64(limit) {
		return false, 0, nil, errMessageTooLarge
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// writeFrame writes one final, masked frame (a client's frames must be masked).
func (c *wsConn) writeFrame(op byte, payload []byte) error {
	header := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = binary.BigEndian.AppendUint64(append(header, 0x80|127), uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(append(header, masked...))
	return err
}

func (c *wsConn) Close() error { return c.conn.Close() }
