// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live path (FR-017, FR-018, FR-046).
//
// A live feeder emits thousands of small events and cares about three things the in-memory
// emitter does not have to think about.
//
// *Batching.* One RPC per event would spend most of its time on round trips. Events are
// buffered and sent with IngestBatch; a batch is not a transaction — the server applies each
// event in its own transaction (internal/server/ingest.go) — so batching changes throughput
// and nothing else about the semantics.
//
// *Retries.* A transport failure is not an answer. Unavailable, deadline exceeded and the
// other transient codes are retried with exponential backoff; InvalidArgument, Unauthenticated
// and PermissionDenied are the server saying no and are returned immediately, because retrying
// a bad token or an unscoped source id only produces the same refusal more slowly.
//
// *Rejections.* A REJECTED result is not a transport failure, and it is not a reason to stop.
// It is counted, logged at warning with its reason code, and handed to the result callback.
// What it is never allowed to be is silent: a feeder whose events are all being refused must
// be able to see that from its own logs and metrics (FR-024).

// Defaults for a ConnectEmitter.
const (
	// DefaultBatchSize is how many events are sent in one IngestBatch call.
	DefaultBatchSize = 100
	// DefaultFlushInterval bounds how long an event waits in the buffer while the feeder is
	// producing slowly.
	DefaultFlushInterval = time.Second
	// DefaultMaxAttempts is how many times a batch is sent before a transport failure is
	// returned to the caller.
	DefaultMaxAttempts = 4
	// DefaultRetryBaseDelay is the first backoff; each further attempt doubles it.
	DefaultRetryBaseDelay = 100 * time.Millisecond
	// DefaultHTTPTimeout bounds one IngestBatch call.
	DefaultHTTPTimeout = 30 * time.Second
)

// Stats are the counters a ConnectEmitter keeps, for a feeder's own metrics and for a test
// that wants to assert on what happened.
type Stats struct {
	// Applied, DuplicateNoop and Rejected count results by status.
	Applied       int64
	DuplicateNoop int64
	Rejected      int64
	// Batches is how many IngestBatch calls were made, Retries how many of those were repeat
	// attempts after a transient failure.
	Batches int64
	Retries int64
	// Dropped counts events discarded because the buffer was full after repeated delivery
	// failures. It should always be zero; anything else is data loss and is logged as such.
	Dropped int64
}

// ConnectEmitter sends a feeder's events to a running graph over ConnectRPC.
type ConnectEmitter struct {
	desc   feeder.Description
	client graphv1connect.IngestServiceClient
	opts   connectOptions

	mu         sync.Mutex
	buf        []*graphv1.EventEnvelope
	oldest     time.Time
	registered bool
	stats      Stats

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

var _ feeder.Emitter = (*ConnectEmitter)(nil)

type connectOptions struct {
	batchSize      int
	flushInterval  time.Duration
	maxAttempts    int
	retryBase      time.Duration
	maxBuffered    int
	httpClient     *http.Client
	client         graphv1connect.IngestServiceClient
	logger         *slog.Logger
	onResult       func(*graphv1.IngestResult)
	connectOptions []connect.ClientOption
}

// ConnectOption configures a ConnectEmitter.
type ConnectOption func(*connectOptions)

// WithBatchSize sets how many events are sent per call. A size of 1 makes Emit synchronous:
// it returns the graph's own IngestResult for that event, which is the mode a feeder uses
// while it is being written.
func WithBatchSize(n int) ConnectOption {
	return func(o *connectOptions) { o.batchSize = n }
}

// WithFlushInterval bounds how long a buffered event waits. Zero or less disables the
// background flusher, leaving Flush the only thing that empties the buffer.
func WithFlushInterval(d time.Duration) ConnectOption {
	return func(o *connectOptions) { o.flushInterval = d }
}

// WithMaxAttempts sets how many times a batch is sent before a transient failure is reported.
func WithMaxAttempts(n int) ConnectOption {
	return func(o *connectOptions) { o.maxAttempts = n }
}

// WithRetryBaseDelay sets the first backoff; each further attempt doubles it.
func WithRetryBaseDelay(d time.Duration) ConnectOption {
	return func(o *connectOptions) { o.retryBase = d }
}

// WithMaxBuffered caps the buffer. Batches that could not be delivered are put back at its
// head, so the cap is what stops an unreachable graph turning into an unbounded queue; past it
// the oldest events are dropped and counted in Stats.Dropped. Default: ten batches.
func WithMaxBuffered(n int) ConnectOption {
	return func(o *connectOptions) { o.maxBuffered = n }
}

// WithHTTPClient replaces the HTTP client. The default attaches the bearer token to every
// request and times a call out after DefaultHTTPTimeout; a replacement must attach the token
// itself.
func WithHTTPClient(c *http.Client) ConnectOption {
	return func(o *connectOptions) { o.httpClient = c }
}

// WithIngestClient replaces the whole generated client, for a caller that has already built
// one — an in-process test server, or a deployment with its own interceptors. baseURL and
// token are then unused.
func WithIngestClient(c graphv1connect.IngestServiceClient) ConnectOption {
	return func(o *connectOptions) { o.client = c }
}

// WithConnectOptions passes options to the generated client, e.g. connect.WithGRPC().
func WithConnectOptions(opts ...connect.ClientOption) ConnectOption {
	return func(o *connectOptions) { o.connectOptions = append(o.connectOptions, opts...) }
}

// WithLogger replaces the logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) ConnectOption {
	return func(o *connectOptions) { o.logger = l }
}

// WithResultFunc registers a callback invoked once per IngestResult, in the order the graph
// returned them. It is how a batching feeder sees what happened to each event; it runs on the
// goroutine that flushed, so it must not block.
func WithResultFunc(fn func(*graphv1.IngestResult)) ConnectOption {
	return func(o *connectOptions) { o.onResult = fn }
}

// NewConnectEmitter builds an emitter against the graph at baseURL, authenticating with token.
//
// baseURL may be written `host:port` or as a full URL. token is a feeder-role credential
// scoped to desc.SourceID; `aisre dev-token --dev --user <name> --roles feeder` mints one
// against a development server. The source is registered lazily, on the first flush, so that
// constructing an emitter never blocks and a feeder can decide for itself when to prove it can
// reach the graph — call Register at startup to do so eagerly.
func NewConnectEmitter(baseURL, token string, desc feeder.Description, opts ...ConnectOption) (*ConnectEmitter, error) {
	if err := desc.Validate(); err != nil {
		return nil, err
	}
	o := connectOptions{
		batchSize:     DefaultBatchSize,
		flushInterval: DefaultFlushInterval,
		maxAttempts:   DefaultMaxAttempts,
		retryBase:     DefaultRetryBaseDelay,
		logger:        slog.Default(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.batchSize < 1 {
		o.batchSize = 1
	}
	if o.maxAttempts < 1 {
		o.maxAttempts = 1
	}
	if o.retryBase <= 0 {
		o.retryBase = DefaultRetryBaseDelay
	}
	if o.maxBuffered <= 0 {
		o.maxBuffered = o.batchSize * 10
	}

	client := o.client
	if client == nil {
		base, err := normalizeBaseURL(baseURL)
		if err != nil {
			return nil, err
		}
		httpClient := o.httpClient
		if httpClient == nil {
			httpClient = &http.Client{
				Transport: &bearerTransport{base: http.DefaultTransport, token: token},
				Timeout:   DefaultHTTPTimeout,
			}
		}
		client = graphv1connect.NewIngestServiceClient(httpClient, base, o.connectOptions...)
	}

	e := &ConnectEmitter{
		desc:   desc,
		client: client,
		opts:   o,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if o.flushInterval > 0 && o.batchSize > 1 {
		go e.flusher()
	} else {
		close(e.done)
	}
	return e, nil
}

// Describe returns the description the emitter was built for.
func (e *ConnectEmitter) Describe() feeder.Description { return e.desc }

// Register declares the feeder's contract to the graph (FR-018). It is called automatically
// before the first batch; call it at startup to fail fast on an unreachable graph or a
// credential that is not scoped to this source.
func (e *ConnectEmitter) Register(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registerLocked(ctx)
}

func (e *ConnectEmitter) registerLocked(ctx context.Context) error {
	if e.registered {
		return nil
	}
	req := connect.NewRequest(e.desc.RegisterRequest())
	err := e.retry(ctx, "register source", func(ctx context.Context) error {
		_, err := e.client.RegisterSource(ctx, req)
		return err
	})
	if err != nil {
		return fmt.Errorf("emit: register source %s: %w", e.desc.SourceID, err)
	}
	e.registered = true
	e.opts.logger.Info("feeder source registered",
		"source", e.desc.SourceID, "kind", e.desc.Kind,
		"ordering", e.desc.Ordering.String(), "schema_version", e.desc.Version())
	return nil
}

// Emit buffers one event and sends the batch when it is full.
//
// With a batch size of 1 the call is synchronous and the returned result is the graph's answer
// for this event. With a larger batch size the graph has not answered yet, so the result is
// nil and a non-nil error means only that an earlier batch could not be delivered; register
// WithResultFunc to see every result.
func (e *ConnectEmitter) Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	if ev == nil {
		return nil, errors.New("emit: nil event")
	}
	// Validating locally turns a round trip into a compile-time-ish error: the feeder author
	// is told which field is wrong by the same validator the server would have used, before
	// anything leaves the process (FR-024).
	if r := feeder.Validate(ev); r != nil {
		return &graphv1.IngestResult{
			EventId:      ev.GetEventId(),
			Status:       graphv1.IngestResult_REJECTED,
			ReasonCode:   r.ReasonCode,
			ReasonDetail: r.ReasonDetail,
		}, r
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) == 0 {
		e.oldest = time.Now()
	}
	e.buf = append(e.buf, ev)
	if len(e.buf) < e.opts.batchSize {
		return nil, nil
	}
	results, err := e.flushLocked(ctx)
	if err != nil {
		return nil, err
	}
	if e.opts.batchSize == 1 && len(results) == 1 {
		return results[0], nil
	}
	return nil, nil
}

// Checkpoint records the feeder's observed extent as a source_checkpoint event (FR-032).
func (e *ConnectEmitter) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	_, err := e.Emit(ctx, feeder.SourceCheckpoint(e.desc, feeder.CheckpointID(e.desc, fact.ExtentTo), fact))
	return err
}

// Flush sends everything buffered and waits for the graph's answers.
func (e *ConnectEmitter) Flush(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.flushLocked(ctx)
	return err
}

// Close flushes and stops the background flusher. A feeder defers it.
func (e *ConnectEmitter) Close(ctx context.Context) error {
	e.stopOnce.Do(func() {
		close(e.stop)
		<-e.done
	})
	return e.Flush(ctx)
}

// Stats returns a snapshot of the counters.
func (e *ConnectEmitter) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats
}

// Buffered is how many events are waiting to be sent.
func (e *ConnectEmitter) Buffered() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.buf)
}

// flusher empties a buffer that has gone stale while the feeder was quiet.
func (e *ConnectEmitter) flusher() {
	defer close(e.done)
	ticker := time.NewTicker(e.opts.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.mu.Lock()
			stale := len(e.buf) > 0 && time.Since(e.oldest) >= e.opts.flushInterval
			if stale {
				ctx, cancel := context.WithTimeout(context.Background(), DefaultHTTPTimeout)
				if _, err := e.flushLocked(ctx); err != nil {
					e.opts.logger.Warn("feeder batch not delivered; it will be retried",
						"source", e.desc.SourceID, "error", err, "buffered", len(e.buf))
				}
				cancel()
			}
			e.mu.Unlock()
		}
	}
}

// flushLocked sends the whole buffer. The caller holds e.mu.
func (e *ConnectEmitter) flushLocked(ctx context.Context) ([]*graphv1.IngestResult, error) {
	if len(e.buf) == 0 {
		return nil, nil
	}
	batch := e.buf
	e.buf = nil
	e.oldest = time.Time{}

	if err := e.registerLocked(ctx); err != nil {
		e.requeueLocked(batch)
		return nil, err
	}

	var results []*graphv1.IngestResult
	err := e.retry(ctx, "ingest batch", func(ctx context.Context) error {
		resp, err := e.client.IngestBatch(ctx, connect.NewRequest(&graphv1.IngestBatchRequest{Events: batch}))
		if err != nil {
			return err
		}
		results = resp.Msg.GetResults()
		return nil
	})
	e.stats.Batches++
	if err != nil {
		e.requeueLocked(batch)
		return nil, fmt.Errorf("emit: ingest %d events for source %s: %w", len(batch), e.desc.SourceID, err)
	}
	e.recordLocked(results)
	return results, nil
}

// requeueLocked puts an undelivered batch back at the head of the buffer, dropping the oldest
// events if that would take it past the cap. Dropping is loud: silent loss in a feeder is the
// one failure an operator cannot diagnose from the graph.
func (e *ConnectEmitter) requeueLocked(batch []*graphv1.EventEnvelope) {
	e.buf = append(batch, e.buf...)
	if overflow := len(e.buf) - e.opts.maxBuffered; overflow > 0 {
		e.stats.Dropped += int64(overflow)
		e.opts.logger.Error("feeder buffer full; dropping the oldest undelivered events",
			"source", e.desc.SourceID, "dropped", overflow, "max_buffered", e.opts.maxBuffered)
		e.buf = e.buf[overflow:]
	}
	if len(e.buf) > 0 && e.oldest.IsZero() {
		e.oldest = time.Now()
	}
}

// recordLocked counts the results, logs the refusals and calls the caller's callback.
func (e *ConnectEmitter) recordLocked(results []*graphv1.IngestResult) {
	for _, result := range results {
		switch result.GetStatus() {
		case graphv1.IngestResult_APPLIED:
			e.stats.Applied++
		case graphv1.IngestResult_DUPLICATE_NOOP:
			e.stats.DuplicateNoop++
		case graphv1.IngestResult_REJECTED:
			e.stats.Rejected++
			e.opts.logger.Warn("event rejected by the graph",
				"source", e.desc.SourceID, "event_id", result.GetEventId(),
				"reason", result.GetReasonCode(), "detail", result.GetReasonDetail())
		case graphv1.IngestResult_STATUS_UNSPECIFIED:
			e.opts.logger.Error("event got no status from the graph",
				"source", e.desc.SourceID, "event_id", result.GetEventId())
		}
		if e.opts.onResult != nil {
			e.opts.onResult(result)
		}
	}
}

// retry runs call with exponential backoff over the transient failures.
func (e *ConnectEmitter) retry(ctx context.Context, what string, call func(context.Context) error) error {
	delay := e.opts.retryBase
	var lastErr error
	for attempt := 1; attempt <= e.opts.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = call(ctx)
		if lastErr == nil {
			return nil
		}
		if !retryable(lastErr) || attempt == e.opts.maxAttempts {
			return lastErr
		}
		e.stats.Retries++
		e.opts.logger.Debug("retrying after a transient failure",
			"source", e.desc.SourceID, "what", what, "attempt", attempt, "error", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return lastErr
}

// retryable distinguishes "the graph did not hear us" from "the graph said no".
//
// A refusal — a malformed request, a token without the feeder role, a source id the token is
// not scoped to — will be refused identically on every attempt, so retrying it only delays the
// error the feeder author has to read. Everything else is treated as transient, including a
// plain network error with no Connect code at all.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return true
	}
	switch connectErr.Code() {
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeAlreadyExists,
		connect.CodePermissionDenied, connect.CodeUnauthenticated, connect.CodeFailedPrecondition,
		connect.CodeOutOfRange, connect.CodeUnimplemented, connect.CodeCanceled:
		return false
	default:
		return true
	}
}

// RedactedValue is what RedactAuthorization and the emitter's own logging put in place of a
// credential. It is a fixed string rather than a prefix of the token: a prefix is still a
// credential to anyone holding the rest of it.
const RedactedValue = "[redacted]"

// RedactAuthorization returns a copy of h with every credential-bearing header replaced by
// RedactedValue. Feeders that log requests for debugging pass their headers through it first;
// nothing in this package ever logs a header without it.
//
// The header set is deliberately wider than `Authorization`: a proxy credential and a cookie
// are credentials too, and a debug log is forever.
func RedactAuthorization(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"} {
		if _, ok := out[http.CanonicalHeaderKey(name)]; ok {
			out.Set(name, RedactedValue)
		}
	}
	return out
}

// bearerTransport attaches the feeder's credential to every request, streams included.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		// net/http may retry a request, so it must not be mutated in place.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// LogValue implements slog.LogValuer so that a transport which lands in a log record — through
// a struct dump, a %+v, or a future debug line — renders as a fact about the credential rather
// than as the credential. slog calls it before anything is written.
func (t *bearerTransport) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("authorization", RedactedValue),
		slog.Bool("token_set", t.token != ""),
	)
}

func (t *bearerTransport) String() string {
	return "bearerTransport{authorization: " + RedactedValue + "}"
}

// normalizeBaseURL accepts `host:port` as well as a full URL, because that is what people
// configure.
//
// Userinfo is stripped. A base URL written `https://feeder:s3cret@graph.internal` puts a
// credential into every error message, every log line and every client span that names the
// endpoint; the graph does not read it in any case, since the credential belongs in the
// Authorization header.
func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("emit: the graph's base URL is empty")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// The error carries the caller's string, which may hold the userinfo that made it
		// unparseable, so it is reported without quoting the URL back.
		return "", fmt.Errorf("emit: the graph's base URL is not a URL: %w", redactURLError(err))
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("emit: %q names no host", parsed.Redacted())
	}
	parsed.User = nil
	return strings.TrimRight(parsed.String(), "/"), nil
}

// redactURLError strips the offending URL out of a *url.Error, which quotes it verbatim.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}
