// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A read-only IMAP client (T079, FR-007, contract §1.1).
//
// # Why both EXAMINE and BODY.PEEK, and why this is not belt-and-braces
//
// RFC 9051 §6.3.2 permits a read-only `SELECT` to change **per-user state**, and `\Seen` is exactly
// that. So a client that opened the mailbox read-only and then fetched `BODY[]` would mark every notice
// as read — in somebody's actual mailbox, the one a person also uses. That is not a theoretical
// violation of FR-007's "changes no state": it is the connector reaching into a colleague's inbox and
// marking forty messages read overnight.
//
// Both are therefore required:
//
//	EXAMINE      opens the mailbox read-only, and the tagged OK must carry [READ-ONLY]
//	BODY.PEEK[]  fetches without setting \Seen
//
// And the `[READ-ONLY]` response code is asserted **before** anything is fetched. A server that
// answered EXAMINE with `[READ-WRITE]` — a proxy, a gateway, a server with a non-conforming EXAMINE — is
// a server this client stops talking to, because the next command would change state. Checking
// afterwards would be checking after the damage.
//
// # Why a client here rather than a dependency
//
// This uses four commands. A general IMAP library is a large amount of code to audit for the one
// property that matters — that nothing it does can write — and the audit would have to be redone on
// every upgrade. Four commands over a TLS connection is something a reviewer can read in one sitting
// and satisfy themselves about, which is the only way FR-007 is a guarantee rather than a hope.
//
// It deliberately implements no command that can write. There is no STORE, no APPEND, no COPY, no
// EXPUNGE and no SELECT here: not disabled, not gated behind a flag — absent. A connector that cannot
// express a write cannot be configured into performing one.

// ErrNotReadOnly reports that the server did not confirm a read-only mailbox.
var ErrNotReadOnly = errors.New("vendornotice: the server did not answer EXAMINE with [READ-ONLY]")

// IMAPAuth is how the client authenticates.
type IMAPAuth struct {
	// User is the account.
	User string
	// Password is a password or app password. Empty with AccessToken set uses XOAUTH2.
	Password string
	// AccessToken is an OAuth 2.0 bearer token for XOAUTH2.
	AccessToken string
}

// IMAPOptions configures the client.
type IMAPOptions struct {
	// Address is `host:port`. Required.
	Address string
	// Auth is the credential. Required.
	Auth IMAPAuth
	// Mailbox is the folder to examine. Required: there is no default, because a default of INBOX
	// would point the connector at a person's own mail rather than the shared mailbox somebody
	// configured (FR-132b).
	Mailbox string
	// Dial connects. Nil dials TLS to Address. A test supplies a scripted connection, which is how
	// the read-only assertions are tested without a server.
	Dial func(ctx context.Context) (net.Conn, error)
	// Timeout bounds the whole conversation. Zero uses DefaultHTTPTimeout.
	Timeout time.Duration
}

// IMAPClient is the four-command read-only client.
type IMAPClient struct {
	opts IMAPOptions
	conn net.Conn
	r    *bufio.Reader
	tag  int
}

// DialIMAP opens the connection, authenticates and EXAMINEs the mailbox.
//
// It returns a client only once `[READ-ONLY]` has been confirmed, so a caller cannot hold a client that
// has not proved the property. That is the ordering FR-007 needs: the proof comes before the fetch.
func DialIMAP(ctx context.Context, opts IMAPOptions) (*IMAPClient, error) {
	if strings.TrimSpace(opts.Address) == "" {
		return nil, errors.New("vendornotice: the IMAP source needs an address")
	}
	if strings.TrimSpace(opts.Mailbox) == "" {
		return nil, errors.New("vendornotice: the IMAP source needs the mailbox to examine. There is " +
			"no default: a default of INBOX would point the connector at whoever's credential it was " +
			"given rather than at the shared mailbox somebody configured (FR-132b)")
	}
	if strings.TrimSpace(opts.Auth.User) == "" ||
		(strings.TrimSpace(opts.Auth.Password) == "" && strings.TrimSpace(opts.Auth.AccessToken) == "") {
		return nil, errors.New("vendornotice: the IMAP source needs a user and either a password or " +
			"an access token")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultHTTPTimeout
	}
	dial := opts.Dial
	if dial == nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: timeout}
			return tls.DialWithDialer(dialer, "tcp", opts.Address, &tls.Config{MinVersion: tls.VersionTLS12})
		}
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("vendornotice: connecting to %s: %w", opts.Address, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	client := &IMAPClient{opts: opts, conn: conn, r: bufio.NewReader(conn)}

	if err := client.greeting(); err != nil {
		return nil, client.failed(err)
	}
	if err := client.login(); err != nil {
		return nil, client.failed(err)
	}
	if err := client.examine(); err != nil {
		return nil, client.failed(err)
	}
	return client, nil
}

// Close ends the session. It sends LOGOUT, which is the only command besides the four that this client
// can express, and it changes nothing.
func (c *IMAPClient) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = c.command("LOGOUT")
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *IMAPClient) failed(err error) error {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	return err
}

// greeting reads the server's opening line.
func (c *IMAPClient) greeting() error {
	line, err := c.line()
	if err != nil {
		return fmt.Errorf("vendornotice: reading the IMAP greeting: %w", err)
	}
	if !strings.HasPrefix(line, "* OK") && !strings.HasPrefix(line, "* PREAUTH") {
		return fmt.Errorf("vendornotice: the IMAP server opened with %q rather than OK", line)
	}
	return nil
}

// login authenticates, with XOAUTH2 where a token was supplied.
func (c *IMAPClient) login() error {
	if token := strings.TrimSpace(c.opts.Auth.AccessToken); token != "" {
		// SASL XOAUTH2: user=<user>^Aauth=Bearer <token>^A^A, base64.
		raw := "user=" + c.opts.Auth.User + "\x01auth=Bearer " + token + "\x01\x01"
		_, err := c.command("AUTHENTICATE XOAUTH2 " + base64.StdEncoding.EncodeToString([]byte(raw)))
		if err != nil {
			return fmt.Errorf("vendornotice: XOAUTH2 authentication failed: %w", err)
		}
		return nil
	}
	if _, err := c.command("LOGIN " + quoteIMAP(c.opts.Auth.User) + " " +
		quoteIMAP(c.opts.Auth.Password)); err != nil {
		return fmt.Errorf("vendornotice: IMAP login failed: %w", err)
	}
	return nil
}

// examine opens the mailbox read-only and asserts the response code.
//
// The assertion is on the **tagged OK's response code**, not on the command having succeeded. A server
// may answer EXAMINE with `OK [READ-WRITE]`, and that answer means the session can write: it is refused
// here, before any fetch, because a check afterwards is a check after the damage.
func (c *IMAPClient) examine() error {
	lines, err := c.command("EXAMINE " + quoteIMAP(c.opts.Mailbox))
	if err != nil {
		return fmt.Errorf("vendornotice: EXAMINE %s failed: %w", c.opts.Mailbox, err)
	}
	tagged := lines[len(lines)-1]
	if !strings.Contains(strings.ToUpper(tagged), "[READ-ONLY]") {
		return fmt.Errorf("%w: it answered %q. RFC 9051 §6.3.2 lets a read-only SELECT change "+
			"per-user state, and \\Seen is exactly that, so a session that cannot prove the mailbox "+
			"is read-only is one this connector stops using rather than fetches from (FR-007)",
			ErrNotReadOnly, tagged)
	}
	return nil
}

// SearchSince returns the UIDs of messages received on or after a date.
func (c *IMAPClient) SearchSince(since time.Time) ([]uint32, error) {
	lines, err := c.command("UID SEARCH SINCE " + since.UTC().Format("02-Jan-2006"))
	if err != nil {
		return nil, fmt.Errorf("vendornotice: UID SEARCH failed: %w", err)
	}
	var uids []uint32
	for _, line := range lines {
		upper := strings.ToUpper(line)
		if !strings.HasPrefix(upper, "* SEARCH") {
			continue
		}
		for _, field := range strings.Fields(line[len("* SEARCH"):]) {
			value, err := strconv.ParseUint(field, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("vendornotice: UID SEARCH returned %q, which is not a uid", field)
			}
			uids = append(uids, uint32(value))
		}
	}
	return uids, nil
}

// Fetch returns one message's raw bytes, fetched with BODY.PEEK so that `\Seen` is not set.
//
// The literal is read and returned to the caller and **never written anywhere**: FR-071 says no message
// body may be stored, on disk, in a log or in any artifact, at any point including on a failed run. The
// caller matches it against the stated rules and drops it.
func (c *IMAPClient) Fetch(uid uint32) ([]byte, error) {
	lines, err := c.commandWithLiterals(fmt.Sprintf("UID FETCH %d (BODY.PEEK[])", uid))
	if err != nil {
		return nil, fmt.Errorf("vendornotice: UID FETCH %d failed: %w", uid, err)
	}
	for _, chunk := range lines.literals {
		return chunk, nil
	}
	return nil, fmt.Errorf("vendornotice: UID FETCH %d returned no body", uid)
}

// command sends one command and reads to its tagged response.
func (c *IMAPClient) command(cmd string) ([]string, error) {
	result, err := c.commandWithLiterals(cmd)
	if err != nil {
		return nil, err
	}
	return result.lines, nil
}

// response is what one command produced.
type response struct {
	lines    []string
	literals [][]byte
}

// commandWithLiterals sends a command and reads its response, including any literal blocks.
func (c *IMAPClient) commandWithLiterals(cmd string) (response, error) {
	if c.conn == nil {
		return response{}, errors.New("vendornotice: the IMAP session is closed")
	}
	c.tag++
	tag := fmt.Sprintf("a%03d", c.tag)
	if _, err := io.WriteString(c.conn, tag+" "+cmd+"\r\n"); err != nil {
		return response{}, fmt.Errorf("writing %s: %w", firstWord(cmd), err)
	}
	var out response
	for {
		line, err := c.line()
		if err != nil {
			return response{}, err
		}
		// A literal is announced as {n} at the end of the line, and the next n bytes are data rather
		// than protocol. Reading them as lines is how a naive client corrupts its own session.
		if size, ok := literalSize(line); ok {
			blob := make([]byte, size)
			if _, err := io.ReadFull(c.r, blob); err != nil {
				return response{}, fmt.Errorf("reading a %d-byte literal: %w", size, err)
			}
			out.literals = append(out.literals, blob)
			// The rest of the line after the literal continues the response.
			rest, err := c.line()
			if err != nil {
				return response{}, err
			}
			out.lines = append(out.lines, line, rest)
			if done, err := taggedResult(rest, tag); done {
				return out, err
			}
			continue
		}
		out.lines = append(out.lines, line)
		if done, err := taggedResult(line, tag); done {
			return out, err
		}
	}
}

// taggedResult reports whether a line is this command's tagged response, and what it said.
func taggedResult(line, tag string) (bool, error) {
	if !strings.HasPrefix(line, tag+" ") {
		return false, nil
	}
	rest := strings.TrimSpace(line[len(tag)+1:])
	upper := strings.ToUpper(rest)
	switch {
	case strings.HasPrefix(upper, "OK"):
		return true, nil
	case strings.HasPrefix(upper, "NO"), strings.HasPrefix(upper, "BAD"):
		return true, fmt.Errorf("the server answered %q", rest)
	default:
		return true, fmt.Errorf("the server answered %q, which is neither OK, NO nor BAD", rest)
	}
}

// line reads one CRLF-terminated protocol line.
func (c *IMAPClient) line() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// literalSize reads a trailing {n} literal announcement.
func literalSize(line string) (int, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	open := strings.LastIndex(line, "{")
	if open < 0 {
		return 0, false
	}
	size, err := strconv.Atoi(line[open+1 : len(line)-1])
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

// quoteIMAP renders an astring as a quoted string.
func quoteIMAP(in string) string {
	escaped := strings.ReplaceAll(in, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

func firstWord(in string) string {
	if idx := strings.Index(in, " "); idx > 0 {
		return in[:idx]
	}
	return in
}
