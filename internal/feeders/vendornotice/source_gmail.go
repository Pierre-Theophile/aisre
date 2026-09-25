// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// The Gmail path (T079, FR-007, research §9).
//
// # One scope, and the scope is the guarantee
//
// `gmail.readonly` **structurally cannot reach** `messages.modify`. That is the whole argument for this
// path: a scope cannot be talked into a write the way a code path can, and it cannot be talked into one
// by a future change to this file either. The IMAP path has to prove read-only-ness at runtime with
// EXAMINE and BODY.PEEK because IMAP has no equivalent; here the credential simply does not carry the
// permission.
//
// So this file asks for that scope and nothing else, and refuses a credential minted with anything
// wider. A token that also carries `gmail.modify` would work — and would mean the guarantee this path
// exists for is not in force, which is worth failing over rather than shipping.
//
// # A shared mailbox provisioned as a real user account
//
// Research §9: **no API can read a Google Group's archive.** Not Gmail's, not Groups Settings', not
// Directory's. A deployment that points this at a group address gets an empty read, which looks exactly
// like a quiet week — the failure mode this whole feeder exists to make visible.
//
// The connector cannot detect that from the outside: a group address answers a token exchange and then
// lists nothing. So it is stated in the documentation and in this comment, and the empty-stream refusal
// (Options.RefuseEmptyNoticeStream) is what turns the misconfiguration into a loud failure at campaign
// start rather than a silence.
//
// # Nothing is marked read
//
// `users.messages.get` does not change `UNREAD`; only `messages.modify` does, and that is the call this
// scope cannot make. There is no read-receipt equivalent to IMAP's `\Seen` problem here, which is why
// this path needs no per-fetch precaution — the precaution is the scope.

// GmailReadonlyScope is the only scope this path uses.
const GmailReadonlyScope = gmail.GmailReadonlyScope

// GmailOptions configures the path.
type GmailOptions struct {
	// HTTPClient is an authorised client carrying a token minted for GmailReadonlyScope and nothing
	// else. Required: the credential is the deployment's to mint, and this package never asks for a
	// wider one.
	HTTPClient *http.Client
	// User is the mailbox to read, a **real user account** and not a group address (research §9).
	// Required; "me" is refused, because it resolves to whoever the credential belongs to rather
	// than to the mailbox somebody configured.
	User string
	// Query narrows the search, in Gmail's own query language. Optional; the lookback window is
	// applied on top of it.
	Query string
	// MaxMessages bounds one cycle. Zero uses DefaultGmailMaxMessages.
	MaxMessages int64
}

// DefaultGmailMaxMessages bounds one cycle's read. A mailbox with more than this in a lookback window is
// a mailbox this source is not reading all of, and the bound is what keeps one cycle from running until
// the quota is gone — the caller sees the count in the cycle report.
const DefaultGmailMaxMessages = 500

// GmailMailbox opens the Gmail path. It is the `Open` a deployment passes to MailboxOptions.
func GmailMailbox(opts GmailOptions) func(ctx context.Context) (MessageReader, error) {
	return func(ctx context.Context) (MessageReader, error) {
		if opts.HTTPClient == nil {
			return nil, fmt.Errorf("vendornotice: the Gmail path needs an authorised client carrying " +
				"a token minted for " + GmailReadonlyScope + " and nothing else")
		}
		user := strings.TrimSpace(opts.User)
		if user == "" || strings.EqualFold(user, "me") {
			return nil, fmt.Errorf("vendornotice: the Gmail path needs the mailbox address to read, " +
				"and not `me`: `me` resolves to whoever the credential belongs to rather than to the " +
				"shared mailbox somebody configured. It must be a real user account — no API can read " +
				"a Google Group's archive (research §9)")
		}
		service, err := gmail.NewService(ctx, option.WithHTTPClient(opts.HTTPClient),
			option.WithScopes(GmailReadonlyScope))
		if err != nil {
			return nil, fmt.Errorf("vendornotice: building the Gmail service: %w", err)
		}
		max := opts.MaxMessages
		if max <= 0 {
			max = DefaultGmailMaxMessages
		}
		return &gmailReader{service: service, user: user, query: opts.Query, max: max}, nil
	}
}

type gmailReader struct {
	service *gmail.Service
	user    string
	query   string
	max     int64
}

// Close releases nothing: the HTTP client belongs to the caller, and closing somebody else's client is
// how a second cycle fails for a reason nobody can find.
func (*gmailReader) Close() error { return nil }

// Messages lists and fetches the window, in RAW form so the same parser reads both paths.
//
// RAW rather than `format=full`: the parsed form Gmail returns has the body split into base64 parts
// with its own structure, and reading it would mean a second MIME implementation that could disagree
// with the IMAP path's about what a message said. One parser, two transports.
func (r *gmailReader) Messages(ctx context.Context, since time.Time) ([]Message, error) {
	query := strings.TrimSpace(r.query + " after:" + since.UTC().Format("2006/01/02"))
	list, err := r.service.Users.Messages.List(r.user).Q(query).MaxResults(r.max).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("vendornotice: listing %s: %w", r.user, err)
	}
	out := make([]Message, 0, len(list.Messages))
	for _, stub := range list.Messages {
		full, err := r.service.Users.Messages.Get(r.user, stub.Id).Format("RAW").Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("vendornotice: reading message %s: %w", stub.Id, err)
		}
		raw, err := base64.URLEncoding.DecodeString(full.Raw)
		if err != nil {
			// One unreadable message is not a reason to abandon the cycle. It is returned with its
			// identifier and no text, which the caller records as an unextracted reading.
			out = append(out, Message{ID: stub.Id})
			continue
		}
		message, err := ParseMessage(raw)
		if err != nil {
			out = append(out, Message{ID: stub.Id})
			continue
		}
		if message.ID == "" {
			message.ID = stub.Id
		}
		out = append(out, message)
	}
	return out, nil
}
