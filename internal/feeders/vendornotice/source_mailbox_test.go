// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// The mailbox source (T079, FR-007, FR-071, FR-072, FR-073).

// imapScript is a fake server that answers the four commands this client can express, and records
// everything it was sent so a test can assert what was *not* sent.
type imapScript struct {
	// readOnly decides whether EXAMINE confirms a read-only mailbox.
	readOnly bool
	// messages are the raw messages, in uid order from 1.
	messages []string
	// sent records every command line the client wrote.
	sent []string
}

func (s *imapScript) serve(t *testing.T) func(ctx context.Context) (net.Conn, error) {
	t.Helper()
	return func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go s.converse(t, server)
		return client, nil
	}
}

func (s *imapScript) converse(t *testing.T, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	write := func(format string, args ...any) {
		if _, err := fmt.Fprintf(conn, format+"\r\n", args...); err != nil {
			return
		}
	}
	write("* OK fake IMAP ready")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.sent = append(s.sent, line)
		tag, rest, _ := strings.Cut(line, " ")
		command := strings.ToUpper(rest)
		switch {
		case strings.HasPrefix(command, "LOGIN"), strings.HasPrefix(command, "AUTHENTICATE"):
			write("%s OK authenticated", tag)
		case strings.HasPrefix(command, "EXAMINE"):
			write("* 2 EXISTS")
			if s.readOnly {
				write("%s OK [READ-ONLY] examined", tag)
			} else {
				write("%s OK [READ-WRITE] selected", tag)
			}
		case strings.HasPrefix(command, "UID SEARCH"):
			uids := make([]string, 0, len(s.messages))
			for i := range s.messages {
				uids = append(uids, fmt.Sprint(i+1))
			}
			write("* SEARCH %s", strings.Join(uids, " "))
			write("%s OK search done", tag)
		case strings.HasPrefix(command, "UID FETCH"):
			fields := strings.Fields(rest)
			if len(fields) < 3 {
				write("%s BAD malformed fetch", tag)
				continue
			}
			var uid int
			if _, err := fmt.Sscanf(fields[2], "%d", &uid); err != nil || uid < 1 || uid > len(s.messages) {
				write("%s NO no such uid", tag)
				continue
			}
			body := s.messages[uid-1]
			write("* %d FETCH (UID %d BODY[] {%d}", uid, uid, len(body))
			if _, err := conn.Write([]byte(body)); err != nil {
				return
			}
			write(")")
			write("%s OK fetch done", tag)
		case strings.HasPrefix(command, "LOGOUT"):
			write("* BYE")
			write("%s OK logged out", tag)
			return
		default:
			// Anything else is a command this client must not be able to express. Answered BAD so
			// that a test asserting "nothing that writes was sent" fails loudly rather than hanging.
			write("%s BAD this server answers only EXAMINE, UID SEARCH, UID FETCH and LOGOUT", tag)
		}
	}
}

const maintenanceEmail = "Message-ID: <acme-1002@acme-gpu.test>\r\n" +
	"From: \"Acme GPU Status\" <status@acme-gpu.test>\r\n" +
	"To: sre@example.test\r\n" +
	"Subject: [ACME-2026-1002] Scheduled maintenance for inference-api\r\n" +
	"Date: Thu, 17 Sep 2026 10:00:00 +0000\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"inference-api will be unavailable from 2026-10-02T02:00:00Z to 2026-10-02T04:00:00Z.\r\n"

const newsletterEmail = "Message-ID: <news-1@marketing.example.test>\r\n" +
	"From: news@marketing.example.test\r\n" +
	"Subject: Our new pricing\r\n" +
	"Date: Thu, 17 Sep 2026 09:00:00 +0000\r\n" +
	"\r\n" +
	"Read about our new pricing tiers.\r\n"

func maintenanceRule() vn.EntryRule {
	return vn.EntryRule{
		Match: regexp.MustCompile(`\[(?P<notice_id>[A-Z]+-\d{4}-\d+)\].*?(?P<product>[a-z-]+) will be ` +
			`unavailable from (?P<start>\S+) to (?P<end>[^.\s]+)`),
		Kind: vn.KindMaintenance,
	}
}

// mailVendor is the allowlist entry with the sender configured. The sender is **matching
// configuration**: it decides which vendor a message is attributed to and is then gone. It is a role
// address at a reserved-for-testing domain, because a person's address is never stored anywhere in this
// repository (FR-072, FR-134).
func mailVendor() vn.Vendor {
	vendor := testVendor()
	vendor.Senders = []string{"status@acme-gpu.test"}
	return vendor
}

func mailboxFromScript(t *testing.T, script *imapScript, rules ...vn.EntryRule) *vn.Mailbox {
	t.Helper()
	list, err := vn.NewAllowlist([]vn.Vendor{mailVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	if len(rules) == 0 {
		rules = []vn.EntryRule{maintenanceRule()}
	}
	source, err := vn.NewMailbox(vn.MailboxOptions{
		Mailboxes: []vn.NamedMailbox{{
			Name: "platform-notices",
			Open: vn.IMAPMailbox(vn.IMAPOptions{
				Address: "imap.example.test:993", Mailbox: "Vendor Notices",
				Auth: vn.IMAPAuth{User: "notices@example.test", Password: "app-password"},
				Dial: script.serve(t),
			}),
		}},
		Allowlist: list, Rules: rules,
		Now: func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	return source
}

// The end to end: EXAMINE, UID SEARCH, UID FETCH with BODY.PEEK, and a typed announcement out.
func TestAMaintenanceEmailBecomesAnAnnouncement(t *testing.T) {
	t.Parallel()

	script := &imapScript{readOnly: true, messages: []string{maintenanceEmail, newsletterEmail}}
	batch, err := mailboxFromScript(t, script).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Vendor != "acme-gpu" || got.Product != "inference-api" {
		t.Errorf("placed as vendor=%q product=%q", got.Vendor, got.Product)
	}
	if got.NoticeID != "ACME-2026-1002" {
		t.Errorf("notice id = %q; the vendor's own identifier is what lets two sources merge (FR-076)",
			got.NoticeID)
	}
	if !got.Window.Start.Equal(time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)) ||
		!got.Window.End.Equal(time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %+v", got.Window)
	}
	if got.Pointer != "mailbox:acme-1002@acme-gpu.test" {
		t.Errorf("pointer = %q, want the message identifier so a human can open the original", got.Pointer)
	}
	// Both messages were looked at; only one was from a configured sender.
	if batch.Considered != 2 {
		t.Errorf("considered = %d, want both messages", batch.Considered)
	}
	// The newsletter is not an extraction failure: nobody configured its sender, so it never claimed
	// to be a notice. Recording it would drown the report and would store a people identifier.
	if len(batch.Unextracted) != 0 {
		t.Errorf("a message from an unconfigured sender was recorded: %+v", batch.Unextracted)
	}
}

// Nothing that could change the mailbox is ever sent. The assertion is on the wire, because that is the
// only place the property is true or false — a code review of this package cannot prove what a future
// version sends.
func TestTheSessionSendsNothingThatCouldChangeTheMailbox(t *testing.T) {
	t.Parallel()

	script := &imapScript{readOnly: true, messages: []string{maintenanceEmail}}
	if _, err := mailboxFromScript(t, script).Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(script.sent) == 0 {
		t.Fatal("the client sent nothing at all")
	}
	conversation := strings.ToUpper(strings.Join(script.sent, "\n"))
	for _, forbidden := range []string{"STORE", "APPEND", "COPY", "MOVE", "EXPUNGE", "SETQUOTA", "CREATE", "DELETE"} {
		if strings.Contains(conversation, forbidden) {
			t.Errorf("the session sent %s. FR-007 means nothing about the mailbox changes, and this "+
				"is somebody's actual mailbox:\n%s", forbidden, strings.Join(script.sent, "\n"))
		}
	}
	// SELECT is forbidden as well as the writes: it opens the mailbox read-write, and RFC 9051
	// §6.3.2 lets even a read-only SELECT change per-user state.
	if strings.Contains(conversation, "SELECT ") && !strings.Contains(conversation, "EXAMINE") {
		t.Error("the session used SELECT rather than EXAMINE")
	}
	if !strings.Contains(conversation, "EXAMINE") {
		t.Error("the session never sent EXAMINE, so the mailbox was not opened read-only")
	}
	// And the fetch peeks. A plain BODY[] sets \\Seen, which marks a colleague's mail as read.
	if !strings.Contains(conversation, "BODY.PEEK[]") {
		t.Errorf("the fetch did not use BODY.PEEK, so it would set \\Seen on every notice:\n%s",
			strings.Join(script.sent, "\n"))
	}
	if strings.Contains(strings.ReplaceAll(conversation, "BODY.PEEK[]", ""), "BODY[]") {
		t.Error("the session fetched BODY[] as well as BODY.PEEK[]")
	}
}

// A server that will not confirm a read-only mailbox is a server this client stops talking to — before
// it fetches anything, because a check afterwards is a check after the damage.
func TestAServerThatWillNotConfirmReadOnlyIsAbandonedBeforeAnyFetch(t *testing.T) {
	t.Parallel()

	// The session itself refuses with the typed error, which is where the property lives.
	script := &imapScript{readOnly: false, messages: []string{maintenanceEmail}}
	_, err := vn.DialIMAP(context.Background(), vn.IMAPOptions{
		Address: "imap.example.test:993", Mailbox: "Vendor Notices",
		Auth: vn.IMAPAuth{User: "notices@example.test", Password: "app-password"},
		Dial: script.serve(t),
	})
	if err == nil {
		t.Fatal("a session against a mailbox that answered [READ-WRITE] was opened")
	}
	if !errors.Is(err, vn.ErrNotReadOnly) {
		t.Errorf("the refusal is %v, want ErrNotReadOnly", err)
	}
	conversation := strings.ToUpper(strings.Join(script.sent, "\n"))
	if strings.Contains(conversation, "FETCH") || strings.Contains(conversation, "SEARCH") {
		t.Errorf("the client searched or fetched after the mailbox refused to be read-only:\n%s",
			strings.Join(script.sent, "\n"))
	}

	// And through the source it is a gap naming the mailbox, not an empty read: a mailbox this
	// connector will not fetch from is a mailbox the checkpoint must not claim to have covered.
	refusing := &imapScript{readOnly: false, messages: []string{maintenanceEmail}}
	batch, err := mailboxFromScript(t, refusing).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a mailbox that answered [READ-WRITE] was read anyway: %+v", batch.Announcements)
	}
	if len(batch.Gaps) != 1 || !strings.Contains(batch.Gaps[0].Why, "READ-ONLY") {
		t.Fatalf("gaps = %+v, want one naming the read-only refusal", batch.Gaps)
	}
	if strings.Contains(strings.ToUpper(strings.Join(refusing.sent, "\n")), "FETCH") {
		t.Error("the source fetched from a mailbox that would not confirm read-only")
	}
}

// A message from a configured sender that no rule claims is recorded as unextracted. This vendor *does*
// send notices here, so a message the rules cannot read is the shape of a notice going missing (FR-074).
func TestAConfiguredSenderWhoseMessageNoRuleClaimsIsRecorded(t *testing.T) {
	t.Parallel()

	const odd = "Message-ID: <acme-odd@acme-gpu.test>\r\n" +
		"From: status@acme-gpu.test\r\n" +
		"Subject: An important update about your account\r\n" +
		"Date: Thu, 17 Sep 2026 10:00:00 +0000\r\n\r\n" +
		"We have changed our terms of service.\r\n"

	script := &imapScript{readOnly: true, messages: []string{odd}}
	batch, err := mailboxFromScript(t, script).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a message no rule claimed was placed: %+v", batch.Announcements)
	}
	if len(batch.Unextracted) != 1 {
		t.Fatalf("unextracted = %+v, want the one message from a configured sender", batch.Unextracted)
	}
	record := batch.Unextracted[0]
	if record.Pointer != "mailbox:acme-odd@acme-gpu.test" {
		t.Errorf("pointer = %q", record.Pointer)
	}
	// And the record carries no content: not the subject, not the body, not the sender.
	rendered := record.Error()
	for _, leaked := range []string{"important update", "terms of service", "status@acme-gpu.test"} {
		if strings.Contains(strings.ToLower(rendered), strings.ToLower(leaked)) {
			t.Errorf("the unextracted record quotes %q: %s", leaked, rendered)
		}
	}
}

// A cancellation is a correction on the same change, not a second announcement saying the opposite
// (FR-067). The mailbox is the only source that can carry one: a status page deletes the row.
func TestACancellationEmailIsACorrection(t *testing.T) {
	t.Parallel()

	const cancellation = "Message-ID: <acme-1002-cancel@acme-gpu.test>\r\n" +
		"From: status@acme-gpu.test\r\n" +
		"Subject: [ACME-2026-1002] Maintenance cancelled\r\n" +
		"Date: Fri, 18 Sep 2026 08:00:00 +0000\r\n\r\n" +
		"The maintenance for inference-api on 2026-10-02T02:00:00Z has been cancelled.\r\n"

	cancel := vn.EntryRule{
		Match: regexp.MustCompile(`\[(?P<notice_id>[A-Z]+-\d{4}-\d+)\].*?cancelled.*?` +
			`(?P<product>[a-z-]+) on (?P<start>\S+) has been cancelled`),
		Kind:  vn.KindMaintenance,
		State: "cancelled",
	}
	script := &imapScript{readOnly: true, messages: []string{cancellation}}
	batch, err := mailboxFromScript(t, script, cancel, maintenanceRule()).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a cancellation was read as an announcement, which would create a second change "+
			"node saying the opposite of the first: %+v", batch.Announcements)
	}
	if len(batch.Corrections) != 1 || batch.Corrections[0].State != "cancelled" {
		t.Fatalf("corrections = %+v", batch.Corrections)
	}
	// It carries the vendor's identifier, which is what lands it on the same change ref.
	if batch.Corrections[0].Announcement.NoticeID != "ACME-2026-1002" {
		t.Errorf("the correction names notice %q", batch.Corrections[0].Announcement.NoticeID)
	}
}

// A quoted-printable multipart message is read, and its attachment is not opened (FR-072).
func TestAMultipartMessageIsReadAndItsAttachmentIsNot(t *testing.T) {
	t.Parallel()

	const multipartEmail = "Message-ID: <acme-mp@acme-gpu.test>\r\n" +
		"From: status@acme-gpu.test\r\n" +
		"Subject: =?utf-8?q?=5BACME-2026-1003=5D_Maintenance?=\r\n" +
		"Date: Thu, 17 Sep 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"inference-api will be unavailable from 2026-10-02T02:00:00Z to=\r\n" +
		" 2026-10-02T04:00:00Z.\r\n" +
		"--b1\r\n" +
		"Content-Type: application/pdf; name=\"notice.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"notice.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQKJSBzZWNyZXQtcGRmLWNvbnRlbnQK\r\n" +
		// A text attachment, which is the case a media-type check alone does not cover: it is skipped
		// by its **disposition**. A vendor who attaches the notice as a .txt is a vendor this
		// connector does not read, which is a stated limit rather than a file it opens.
		"--b1\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Disposition: attachment; filename=\"notice.txt\"\r\n\r\n" +
		"attached-text-not-to-be-read\r\n" +
		"--b1--\r\n"

	message, err := vn.ParseMessage([]byte(multipartEmail))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	// The subject's encoded words are decoded, because a rule matches what a human reads.
	if !strings.Contains(message.Subject, "[ACME-2026-1003]") {
		t.Errorf("subject = %q, want the RFC 2047 encoding decoded", message.Subject)
	}
	// The soft line break of quoted-printable is undone, so the window is one token again.
	if !strings.Contains(message.Text, "to 2026-10-02T04:00:00Z") {
		t.Errorf("text = %q, want the quoted-printable soft break resolved", message.Text)
	}
	// The attachment was not opened. Its decoded content must appear nowhere.
	for _, leaked := range []string{"secret-pdf-content", "JVBERi", "attached-text-not-to-be-read"} {
		if strings.Contains(message.Text, leaked) {
			t.Errorf("an attachment was read (%q): %q", leaked, message.Text)
		}
	}

	script := &imapScript{readOnly: true, messages: []string{multipartEmail}}
	batch, err := mailboxFromScript(t, script).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("the multipart notice produced %+v", batch)
	}
	if !batch.Announcements[0].Window.End.Equal(time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %+v", batch.Announcements[0].Window)
	}
}

// A mailbox with no extraction rules would read every message and place none. Refused at construction,
// because the alternative is a feeder interpreting prose (FR-073).
func TestAMailboxWithNoRulesIsRefused(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{mailVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	open := func(context.Context) (vn.MessageReader, error) { return nil, errors.New("unused") }
	one := []vn.NamedMailbox{{Name: "platform-notices", Open: open}}
	if _, err := vn.NewMailbox(vn.MailboxOptions{Mailboxes: one, Allowlist: list}); err == nil {
		t.Error("a mailbox with no rules was accepted")
	}
	if _, err := vn.NewMailbox(vn.MailboxOptions{
		Mailboxes: one, Rules: []vn.EntryRule{maintenanceRule()},
	}); err == nil {
		t.Error("a mailbox with no allowlist was accepted")
	}
	// No mailboxes at all, and a mailbox with no name: a gap has to say which one could not be read,
	// and "the mailbox" is not an answer when notices arrive in several (FR-132a).
	if _, err := vn.NewMailbox(vn.MailboxOptions{
		Allowlist: list, Rules: []vn.EntryRule{maintenanceRule()},
	}); err == nil {
		t.Error("a mailbox source with no mailboxes was accepted")
	}
	if _, err := vn.NewMailbox(vn.MailboxOptions{
		Mailboxes: []vn.NamedMailbox{{Open: open}},
		Allowlist: list, Rules: []vn.EntryRule{maintenanceRule()},
	}); err == nil {
		t.Error("an unnamed mailbox was accepted")
	}
}

// Per-user delivery is a first-class path (FR-132a): notices arrive in individuals' mailboxes, so the
// source reads several. One of them being unreachable must lose neither the others' work nor the fact
// that it was unreachable.
func TestOneUnreachableMailboxDoesNotLoseTheOthersNorHideItself(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{mailVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	working := &imapScript{readOnly: true, messages: []string{maintenanceEmail}}
	source, err := vn.NewMailbox(vn.MailboxOptions{
		Mailboxes: []vn.NamedMailbox{
			{Name: "essential-contact-1", Open: func(context.Context) (vn.MessageReader, error) {
				return nil, errors.New("dial tcp: i/o timeout")
			}},
			{Name: "platform-notices", Open: vn.IMAPMailbox(vn.IMAPOptions{
				Address: "imap.example.test:993", Mailbox: "Vendor Notices",
				Auth: vn.IMAPAuth{User: "notices@example.test", Password: "app-password"},
				Dial: working.serve(t),
			})},
		},
		Allowlist: list, Rules: []vn.EntryRule{maintenanceRule()},
		Now: func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}

	batch, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("the working mailbox's notice was lost: %+v", batch)
	}
	if len(batch.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want the unreachable mailbox", batch.Gaps)
	}
	if !strings.Contains(string(batch.Gaps[0].Source), "essential-contact-1") {
		t.Errorf("the gap does not say which mailbox: %+v", batch.Gaps[0])
	}
	if batch.Gaps[0].Why == "" {
		t.Error("the gap carries no reason")
	}

	// And the gap reaches the cycle marker, which is what reaches the checkpoint.
	poller, err := vn.NewPoller(vn.PollerOptions{
		Sources: []vn.Announcer{source}, Now: func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	found := false
	for _, payload := range drain(t, poller) {
		if payload.Kind == vn.PayloadCycleMarker &&
			strings.Contains(string(payload.Bytes), "essential-contact-1") {
			found = true
		}
	}
	if !found {
		t.Error("a mailbox that could not be read did not reach the cycle marker, so the checkpoint " +
			"would claim an extent covering a mailbox nobody opened (FR-075)")
	}
}

// There is no default mailbox. A default of INBOX would point the connector at whoever's credential it
// was given rather than at the shared mailbox somebody configured (FR-132b).
func TestTheIMAPPathHasNoDefaultMailbox(t *testing.T) {
	t.Parallel()

	_, err := vn.DialIMAP(context.Background(), vn.IMAPOptions{
		Address: "imap.example.test:993",
		Auth:    vn.IMAPAuth{User: "notices@example.test", Password: "app-password"},
	})
	if err == nil {
		t.Fatal("a session with no mailbox named was opened")
	}
	if !strings.Contains(err.Error(), "INBOX") {
		t.Errorf("the refusal does not explain why there is no default: %v", err)
	}
}

// noopHTTPClient is a client that would fail any request, which is enough for the construction checks:
// they must refuse before anything is sent.
func noopHTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no request should have been made")
	})}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// The Gmail path refuses `me`, which resolves to whoever the credential belongs to rather than to the
// configured mailbox — and refuses no credential at all.
func TestTheGmailPathRefusesMeAndAMissingCredential(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := vn.GmailMailbox(vn.GmailOptions{User: "notices@example.test"})(ctx); err == nil {
		t.Error("the Gmail path opened with no authorised client")
	}
	for _, user := range []string{"", "me", "  ME  "} {
		open := vn.GmailMailbox(vn.GmailOptions{HTTPClient: noopHTTPClient(), User: user})
		if _, err := open(ctx); err == nil {
			t.Errorf("the Gmail path accepted user %q", user)
		}
	}
}

// The body never reaches an event, a payload or the cycle report — the whole of T084 and FR-071, asserted
// on what a run actually produces rather than on the type holding no field for it.
//
// The type-level argument is in sanitise_test.go: Notice has no member a body could live in. This is the
// other half, because the mailbox path necessarily *does* hold the text long enough to match it, and a
// property nothing checks end to end is a property that holds until somebody adds a log line.
func TestTheBodyReachesNoEventNoPayloadAndNoReport(t *testing.T) {
	t.Parallel()

	const secret = "PLEASE-DO-NOT-STORE-THIS-SENTENCE"
	body := "Message-ID: <acme-1004@acme-gpu.test>\r\n" +
		"From: status@acme-gpu.test\r\n" +
		"Subject: [ACME-2026-1004] " + secret + "\r\n" +
		"Date: Thu, 17 Sep 2026 10:00:00 +0000\r\n\r\n" +
		secret + ": inference-api will be unavailable from 2026-10-02T02:00:00Z to " +
		"2026-10-02T04:00:00Z. " + secret + "\r\n"

	script := &imapScript{readOnly: true, messages: []string{body}}
	batch, err := mailboxFromScript(t, script).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("the notice was not placed, so this test is asserting nothing: %+v", batch)
	}

	// Through the poller, which is what a recording writes to disk.
	poller, err := vn.NewPoller(vn.PollerOptions{
		Sources: []vn.Announcer{&fakeSource{kind: vn.SourceMailbox, batches: []vn.Batch{batch}}},
		// The same fixed clock the feeder runs on, so the payload's instant is not in the feeder's
		// future — which is FR-063's refusal and has nothing to do with what this test is about.
		Now: func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	payloads := drain(t, poller)
	for _, payload := range payloads {
		if strings.Contains(string(payload.Bytes), secret) {
			t.Fatalf("a %s payload carries the message body: %s", payload.Kind, payload.Bytes)
		}
	}

	// And through the feeder, which is what reaches the graph.
	f := newTestFeeder(t, false)
	em := &recorder{}
	if err := f.Run(context.Background(), &sliceSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.events) == 0 {
		t.Fatal("the run emitted nothing, so this test is asserting nothing")
	}
	for _, ev := range em.events {
		if rendered := ev.String(); strings.Contains(rendered, secret) {
			t.Fatalf("an event carries the message body: %s", rendered)
		}
	}
	if note := f.Report().Note(); strings.Contains(note, secret) {
		t.Fatalf("the cycle report carries the message body: %s", note)
	}

	// The Message's own rendering elides it too, which is the guard against the ordinary mistake — a
	// log line about a message somebody adds later.
	message, err := vn.ParseMessage([]byte(body))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if rendered := message.String(); strings.Contains(rendered, secret) {
		t.Errorf("a message renders its own body: %s", rendered)
	}
	if !strings.Contains(message.String(), "acme-1004@acme-gpu.test") {
		t.Errorf("a message renders nothing useful either: %s", message.String())
	}
}

// A run that fails still stores no body. FR-071 says "at any point including on a failed or aborted
// run", and the error path is where a body most easily escapes: an error that quoted what it could not
// parse would write the notice into a log.
func TestAFailedRunStoresNoBodyEither(t *testing.T) {
	t.Parallel()

	const secret = "PLEASE-DO-NOT-STORE-THIS-SENTENCE"
	body := "Message-ID: <acme-1005@acme-gpu.test>\r\n" +
		"From: status@acme-gpu.test\r\n" +
		"Subject: [ACME-2026-1005] " + secret + "\r\n" +
		"Date: Thu, 17 Sep 2026 10:00:00 +0000\r\n\r\n" +
		secret + ": inference-api will be unavailable from whenever we feel like it. " + secret + "\r\n"

	script := &imapScript{readOnly: true, messages: []string{body}}
	batch, err := mailboxFromScript(t, script).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// No rule places it, so it is an unextracted record — the failure path.
	if len(batch.Unextracted) != 1 {
		t.Fatalf("the message was placed after all, so this test is asserting nothing: %+v", batch)
	}
	if rendered := batch.Unextracted[0].Error(); strings.Contains(rendered, secret) {
		t.Fatalf("the extraction failure quotes the body: %s", rendered)
	}

	poller, err := vn.NewPoller(vn.PollerOptions{
		Sources: []vn.Announcer{&fakeSource{kind: vn.SourceMailbox, batches: []vn.Batch{batch}}},
		// The same fixed clock the feeder runs on, so the payload's instant is not in the feeder's
		// future — which is FR-063's refusal and has nothing to do with what this test is about.
		Now: func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	for _, payload := range drain(t, poller) {
		if strings.Contains(string(payload.Bytes), secret) {
			t.Fatalf("a payload recorded on the failure path carries the body: %s", payload.Bytes)
		}
	}
}
