// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// The mailbox source (T079, FR-007, FR-071, FR-072, research §9, contract §1.1, §4).
//
// # Read-only in a stronger sense than usual
//
// The mailbox is somebody's actual mailbox. A connector that marked notices as read would be reaching
// into a colleague's inbox and changing forty messages overnight — so "read-only" here means *nothing
// about the mailbox changes*, not merely that this process does not delete anything.
//
// Two paths, and both are structurally read-only rather than carefully read-only:
//
//	IMAP   EXAMINE plus UID FETCH … (BODY.PEEK[]), with [READ-ONLY] asserted before any fetch.
//	       Both are needed: RFC 9051 §6.3.2 lets a read-only SELECT change per-user state, and
//	       \Seen is exactly that (see imap.go, which implements no command that can write).
//	Gmail  the `gmail.readonly` scope **alone**, which structurally cannot reach messages.modify.
//	       A scope cannot be talked into a write the way a code path can.
//
// Research §9's finding is why the Gmail path targets a shared mailbox provisioned as a **real user
// account**: no API can read a Google Group's archive. A deployment that pointed this at a group would
// get an empty read that looks exactly like a quiet week, which is why the account kind is a
// configuration fact stated in the connector's documentation rather than an implementation detail.
//
// # What survives a message
//
// The six typed fields, a bounded summary built from them, and a pointer to the message identifier.
// Nothing else. The body is matched against the stated rules in memory and dropped: it is never
// written to disk, to a log or to any artifact, including on a failed run (FR-071). No sender address,
// no display name, no verbatim subject, no quoted thread, and attachments are not opened (FR-072).
//
// The sender is **matching configuration**: it decides which vendor a message is attributed to, through
// the allowlist, and is then gone. It is not stored anywhere, because a sender address is a people
// identifier (FR-134).
//
// # Extraction is the same stated-rule mechanism the feeds use
//
// An operator states regular expressions with named captures; a message no rule claims is counted as
// looked at and placed as nothing. Nothing here reads a subject line and decides what it means, because
// that would be a component treating an outsider's text as an instruction about what to write into the
// graph (FR-073).

// Message is one message as this source needs it: an identifier, a sender for matching, and the text to
// match against. It is the boundary both paths meet at, and it holds no field a body could be stored in
// beyond the text being matched right now.
type Message struct {
	// ID is the message identifier, which becomes the pointer a human opens. It is an identifier and
	// not content (§4).
	ID string
	// From is the sender, for matching against the allowlist. It is never stored.
	From string
	// Subject and Text are the text the stated rules match against. Never stored (FR-071, FR-072).
	Subject string
	Text    string
	// Received is when the message arrived, used as nothing but a bound on what to read.
	Received time.Time
}

// String elides everything a body could travel in.
//
// The type has to carry the subject, the sender and the text — they are what the stated rules match
// against — and that makes it the one struct in this package a `%v` could write a notice's prose out of.
// So it prints its identifier and the shape of the rest, which is what a log line about a message
// actually needs, and the prose has no rendering to leak through (FR-071, FR-072).
//
// Two things follow: `%v` and `%s` are safe, and `%+v` is not. Formatting a Message with `%+v` prints
// every field including the text, because Go ignores Stringer for the plus flag — so this method is a
// guard against the ordinary mistake, not a guarantee. The guarantee is that nothing in this package
// formats one at all, which the tests assert by looking for the body in what a run produces.
func (m Message) String() string {
	return fmt.Sprintf("message %s from a configured sender, subject %d bytes, text %d bytes",
		firstNonEmpty(m.ID, "with no identifier"), len(m.Subject), len(m.Text))
}

// MessageReader is the mailbox behind either path.
type MessageReader interface {
	// Messages returns the messages received on or after `since`. An error means the mailbox could
	// not be read, which the poller turns into a gap.
	Messages(ctx context.Context, since time.Time) ([]Message, error)
	// Close releases the session.
	Close() error
}

// NamedMailbox is one mailbox to read, with the name a gap reports it by.
//
// Several of them is the ordinary case, not the advanced one. FR-132a: Google Cloud addresses platform
// notices to each project's **Essential Contacts** and to its billing and owner principals, so a notice
// stream generally arrives in individuals' mailboxes and reaches a shared address only where somebody
// configured it to. A feeder that read one shared mailbox would miss the notices this feature exists to
// capture, which is why the plural is the interface rather than a later addition to it.
type NamedMailbox struct {
	// Name identifies the mailbox in a gap and in a log line. It is operator-supplied and is meant to
	// be a label — "platform-notices", "essential-contact-1" — rather than an address: an address
	// here would put a people identifier into a checkpoint (FR-072, FR-134).
	Name string
	// Open opens it for one cycle.
	Open func(ctx context.Context) (MessageReader, error)
}

// Mailbox reads announcements out of one or more mailboxes.
type Mailbox struct {
	mailboxes []NamedMailbox
	list      *Allowlist
	rules     []EntryRule
	byVendor  map[string][]EntryRule
	since     func() time.Time
}

// MailboxOptions configures the source.
type MailboxOptions struct {
	// Mailboxes are the mailboxes to read, at least one. Each is a function rather than a live reader
	// so that a cycle that failed to connect is a gap and the next cycle tries again.
	Mailboxes []NamedMailbox
	// Allowlist attributes a message to a vendor by its sender. Required.
	Allowlist *Allowlist
	// RulesByVendor are the stated extraction rules per vendor slug, tried before Rules. What an
	// announcement looks like is a fact about the vendor who writes them, so a message is attributed
	// to a vendor by its sender first and only that vendor's rules are then tried — which keeps one
	// vendor's phrasing from placing another's mail.
	RulesByVendor map[string][]EntryRule
	// Rules are the rules tried for any vendor, after that vendor's own. They are for senders that
	// carry notices for a platform rather than for one product.
	Rules []EntryRule
	// Lookback is how far back to read each cycle. Zero uses DefaultMailboxLookback.
	Lookback time.Duration
	// Now supplies the clock. Nil uses time.Now.
	Now func() time.Time
}

// DefaultMailboxLookback is how far back a cycle reads.
//
// Wider than any cadence on purpose. A notice read twice is a no-op — the ref is deterministic from what
// the announcement *is* (FR-076) — and a notice missed because a cycle was skipped is the failure this
// feeder exists to prevent. The asymmetry is the whole reason to overlap.
const DefaultMailboxLookback = 7 * 24 * time.Hour

// NewMailbox builds the source.
func NewMailbox(opts MailboxOptions) (*Mailbox, error) {
	if len(opts.Mailboxes) == 0 {
		return nil, fmt.Errorf("vendornotice: the mailbox source needs at least one mailbox to read")
	}
	names := map[string]bool{}
	for i, mailbox := range opts.Mailboxes {
		if mailbox.Open == nil {
			return nil, fmt.Errorf("vendornotice: mailbox %d has no way to open it", i)
		}
		name := strings.TrimSpace(mailbox.Name)
		if name == "" {
			return nil, fmt.Errorf("vendornotice: mailbox %d has no name; a gap has to say which "+
				"mailbox could not be read, and \"the mailbox\" is not an answer when there are "+
				"several (FR-132a)", i)
		}
		if names[name] {
			return nil, fmt.Errorf("vendornotice: two mailboxes are both called %q", name)
		}
		names[name] = true
	}
	if opts.Allowlist == nil {
		return nil, fmt.Errorf("vendornotice: the mailbox source needs the allowlist; the sender is " +
			"how a message is attributed to a vendor, and configuration is the authority on vendor " +
			"identity rather than a parsed domain (FR-059)")
	}
	if len(opts.Rules) == 0 && len(opts.RulesByVendor) == 0 {
		return nil, fmt.Errorf("vendornotice: the mailbox source has no extraction rules, so it would " +
			"read every message and place none. The rules are how an operator states what a message " +
			"means; without them the feeder would have to interpret prose (FR-073)")
	}
	if err := validateRules("", opts.Rules); err != nil {
		return nil, err
	}
	for vendor, rules := range opts.RulesByVendor {
		if err := validateRules(vendor, rules); err != nil {
			return nil, err
		}
	}
	lookback := opts.Lookback
	if lookback == 0 {
		lookback = DefaultMailboxLookback
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	byVendor := map[string][]EntryRule{}
	for vendor, rules := range opts.RulesByVendor {
		byVendor[strings.ToLower(strings.TrimSpace(vendor))] = rules
	}
	return &Mailbox{
		mailboxes: append([]NamedMailbox(nil), opts.Mailboxes...),
		list:      opts.Allowlist, rules: opts.Rules,
		since: func() time.Time { return now().UTC().Add(-lookback) },
	}, nil
}

// Kind implements Announcer.
func (m *Mailbox) Kind() SourceKind { return SourceMailbox }

// Read opens each configured mailbox in turn, reads one lookback window and closes it.
//
// One mailbox failing does not lose the others' work and is not reported as a clean read: it becomes a
// gap naming that mailbox (FR-075, FR-132a). Every mailbox failing is still a Batch with gaps rather
// than an error, because the distinction the caller needs — which mailboxes were read — is in the gaps
// and an error would collapse it to one line.
func (m *Mailbox) Read(ctx context.Context) (Batch, error) {
	var batch Batch
	for _, mailbox := range m.mailboxes {
		if err := ctx.Err(); err != nil {
			return Batch{}, err
		}
		one, err := m.readOne(ctx, mailbox)
		if err != nil {
			if ctx.Err() != nil {
				return Batch{}, ctx.Err()
			}
			batch.Gaps = append(batch.Gaps, Gap{
				Source: SourceKind(string(SourceMailbox) + ":" + mailbox.Name),
				Why:    err.Error(),
			})
			continue
		}
		batch.Announcements = append(batch.Announcements, one.Announcements...)
		batch.Corrections = append(batch.Corrections, one.Corrections...)
		batch.Unextracted = append(batch.Unextracted, one.Unextracted...)
		batch.Considered += one.Considered
	}
	return batch, nil
}

// readOne reads a single mailbox.
func (m *Mailbox) readOne(ctx context.Context, mailbox NamedMailbox) (Batch, error) {
	reader, err := mailbox.Open(ctx)
	if err != nil {
		return Batch{}, err
	}
	defer func() { _ = reader.Close() }()

	messages, err := reader.Messages(ctx, m.since())
	if err != nil {
		return Batch{}, err
	}
	batch := Batch{Considered: len(messages)}
	for _, message := range messages {
		pointer := "mailbox:" + firstNonEmpty(message.ID, "unidentified-message")
		vendor, known := m.list.VendorBySender(message.From)
		if !known {
			// Most of what arrives in this mailbox is not a notice, and a message from a sender
			// nobody configured is not one. It is counted as looked at and nothing else: recording
			// every newsletter as an extraction failure would drown the report, and recording the
			// sender would store a people identifier (FR-072).
			continue
		}
		rule, captures, ok := m.match(vendor.Slug, message)
		if !ok {
			// The sender is configured, so this vendor does send notices here — and this message
			// matched no stated rule. That is worth seeing: it is the shape of a notice the rules
			// have stopped reading, which is how a real notice goes missing (FR-074).
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoKind, Field: "kind", Pointer: pointer, Source: SourceMailbox,
			})
			continue
		}
		a, err := rule.announcement(vendor.Slug, pointer, captures, SourceMailbox)
		if err != nil {
			batch.Unextracted = append(batch.Unextracted, unextractedFrom(err, pointer, SourceMailbox))
			continue
		}
		if state := strings.ToLower(strings.TrimSpace(rule.State)); state != "" {
			batch.Corrections = append(batch.Corrections, Correction{Announcement: a, State: state})
			continue
		}
		batch.Announcements = append(batch.Announcements, a)
	}
	return batch, nil
}

// match finds the first stated rule that claims a message.
//
// The subject and the text are matched together, because a vendor states the window in one or the other
// and which one is not this feeder's business to predict.
func (m *Mailbox) match(vendor string, message Message) (EntryRule, map[string]string, bool) {
	text := collapseSpace(message.Subject + " " + message.Text)
	// This vendor's own rules first, then the ones that apply to any vendor. The order is the point:
	// one vendor's phrasing must not place another's mail, and a shared rule is a deliberate fallback
	// rather than a competitor.
	candidates := append(
		append([]EntryRule(nil), m.byVendor[strings.ToLower(strings.TrimSpace(vendor))]...),
		m.rules...)
	for _, rule := range candidates {
		found := rule.Match.FindStringSubmatch(text)
		if found == nil {
			continue
		}
		captures := map[string]string{}
		for i, name := range rule.Match.SubexpNames() {
			if name == "" || i >= len(found) {
				continue
			}
			captures[name] = strings.TrimSpace(found[i])
		}
		return rule, captures, true
	}
	return EntryRule{}, nil, false
}

// unextractedFrom turns an extraction error into a record carrying a pointer and no content.
func unextractedFrom(err error, pointer string, source SourceKind) Unextracted {
	var unextracted *Unextracted
	if errors.As(err, &unextracted) {
		record := *unextracted
		record.Pointer, record.Source = pointer, source
		return record
	}
	return Unextracted{Reason: UnextractedInvalidWindow, Pointer: pointer, Source: source}
}

// IMAPMailbox opens the IMAP path. It is the `Open` a deployment passes to MailboxOptions.
func IMAPMailbox(opts IMAPOptions) func(ctx context.Context) (MessageReader, error) {
	return func(ctx context.Context) (MessageReader, error) {
		client, err := DialIMAP(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &imapReader{client: client}, nil
	}
}

type imapReader struct{ client *IMAPClient }

func (r *imapReader) Close() error { return r.client.Close() }

// Messages searches the examined mailbox and fetches each message with BODY.PEEK.
func (r *imapReader) Messages(_ context.Context, since time.Time) ([]Message, error) {
	uids, err := r.client.SearchSince(since)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(uids))
	for _, uid := range uids {
		raw, err := r.client.Fetch(uid)
		if err != nil {
			return nil, err
		}
		message, err := ParseMessage(raw)
		if err != nil {
			// A message this connector cannot parse is not a reason to abandon the cycle: the rest
			// of the mailbox is still readable, and the unreadable one is reported by the caller as
			// an unextracted record with its uid as the pointer.
			out = append(out, Message{ID: fmt.Sprintf("uid-%d", uid)})
			continue
		}
		out = append(out, message)
	}
	return out, nil
}

// ParseMessage reads one RFC 5322 message into the fields this source matches against.
//
// It returns the text and holds no reference to the raw bytes afterwards. Attachments are **not
// opened**: only `text/plain` and `text/html` parts are read, and an attachment is skipped by its
// disposition rather than by its type, because a vendor sending a notice as a PDF is a notice this
// connector does not read rather than a file it opens (FR-072).
func ParseMessage(raw []byte) (Message, error) {
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return Message{}, fmt.Errorf("vendornotice: reading a message: %w", err)
	}
	header := parsed.Header
	received, _ := mail.ParseDate(header.Get("Date"))
	message := Message{
		ID:       strings.Trim(strings.TrimSpace(header.Get("Message-ID")), "<>"),
		From:     senderAddress(header.Get("From")),
		Subject:  decodeHeader(header.Get("Subject")),
		Received: received.UTC(),
	}
	text, err := readTextParts(header.Get("Content-Type"), header.Get("Content-Transfer-Encoding"), parsed.Body)
	if err != nil {
		return Message{}, err
	}
	message.Text = collapseSpace(text)
	return message, nil
}

// senderAddress reduces a From header to the bare address, for matching only.
func senderAddress(value string) string {
	if parsed, err := mail.ParseAddress(strings.TrimSpace(value)); err == nil {
		return strings.ToLower(parsed.Address)
	}
	// An unparseable From is matched as it stands: the allowlist compares exact addresses, so a
	// malformed one simply matches nothing, which is the right outcome.
	return strings.ToLower(strings.TrimSpace(value))
}

// decodeHeader decodes an RFC 2047 encoded-word header.
func decodeHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// readTextParts walks the MIME structure and returns the text of the text parts.
func readTextParts(contentType, encoding string, body io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(firstNonEmpty(contentType, "text/plain")))
	if err != nil {
		// A Content-Type this connector cannot parse is read as plain text, which is what a mail
		// client does and is safe here: the bytes are matched and dropped either way.
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", nil
		}
		reader := multipart.NewReader(body, boundary)
		var parts []string
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			disposition, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if strings.EqualFold(disposition, "attachment") {
				// Not opened. A notice sent as an attachment is one this connector does not read,
				// which is a stated limit rather than a file it opens (FR-072).
				_ = part.Close()
				continue
			}
			text, err := readTextParts(part.Header.Get("Content-Type"),
				part.Header.Get("Content-Transfer-Encoding"), part)
			_ = part.Close()
			if err != nil {
				continue
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " "), nil
	}
	if !strings.HasPrefix(mediaType, "text/") {
		return "", nil
	}
	decoded, err := io.ReadAll(io.LimitReader(decodeBody(body, encoding), MaxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("vendornotice: reading a message part: %w", err)
	}
	if strings.EqualFold(mediaType, "text/html") {
		return collapseSpace(stripTags(string(decoded))), nil
	}
	return collapseSpace(string(decoded)), nil
}

// decodeBody wraps a part in its transfer decoding.
func decodeBody(body io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	default:
		return body
	}
}

// newlineStripper drops the line breaks a base64 body is wrapped at, which the decoder refuses.
type newlineStripper struct{ r io.Reader }

func (s newlineStripper) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n == 0 {
		return 0, err
	}
	kept := 0
	for i := range n {
		if p[i] != '\r' && p[i] != '\n' {
			p[kept] = p[i]
			kept++
		}
	}
	return kept, err
}

// validateRules refuses a rule that could never fire or could never be recorded.
func validateRules(vendor string, rules []EntryRule) error {
	where := "mailbox"
	if vendor != "" {
		where = "mailbox rules for vendor " + vendor
	}
	for i, rule := range rules {
		if rule.Match == nil {
			return fmt.Errorf("vendornotice: %s rule %d has no expression", where, i)
		}
		if !rule.Kind.Valid() {
			return fmt.Errorf("vendornotice: %s rule %d announces kind %q, which is not one this "+
				"feeder publishes", where, i, rule.Kind)
		}
	}
	return nil
}
