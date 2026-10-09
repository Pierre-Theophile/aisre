// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/container/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	// The AuditLog message type. Importing it registers it, so that an audit entry's `protoPayload` —
	// an Any — can be written as the JSON the API returns; without it protojson refuses every entry
	// and the general change stream would read nothing.
	"google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live poller (T185): the feeder.Source that reads GCP.
//
// It turns the area readers of transport.go into the payloads `Feeder.apply` dispatches on, in the
// shapes the recorded fixtures already carry — each area's list response as JSON, then one `poll`
// marker closing the cycle. So a live run and a replay of its recording go through one decoder, and a
// recording of this poller is a fixture `fixture verify` can take.
//
// What it does not do is decide anything. Every interpretation — what a revision means, when a change
// happened, what an audit entry targets — stays in the feeder, where the fixtures test it. The poller
// is a reader with a clock: which calls, in which order, over which window, and what to say when one
// of them failed.
//
// # The credential comes from the gate, through Connect
//
// The transport is built on the first Next, from Connect, and not before. Feeder.Run proves the gate
// before it asks the source for anything, so there is no ordering in which a GCP client exists before
// the check that makes it safe (FR-004).
//
// # A failed read is a partial cycle, never a quiet one
//
// One area failing does not end the run and does not vanish either: the cycle's marker says
// `partial`, names the read and why, and the checkpoint carries it (FR-012). A read the budget refused
// says `quota_exhausted` — "we ran out of our share" — which is never evidence of absence. Only a
// cancelled context ends the run.

// DefaultPollInterval is the time between cycle starts when none is configured.
const DefaultPollInterval = 5 * time.Minute

// DefaultAuditLookback is how far back the first cycle reads the audit stream.
const DefaultAuditLookback = time.Hour

// PollerOptions configures a live poller.
type PollerOptions struct {
	// Connect builds the transport, once, from the credential the gate proved. Required.
	Connect func(context.Context) (*Transport, error)
	// Scope is what is read. Projects and Regions are both required: no code assumes either (FR-131).
	Scope Scope
	// Interval is the time between cycle starts. Zero uses DefaultPollInterval.
	Interval time.Duration
	// Cycles stops after this many cycles; zero runs until the context is cancelled.
	Cycles int
	// AuditLookback is how far back the first cycle reads the audit stream. Zero uses
	// DefaultAuditLookback.
	AuditLookback time.Duration
	// AuditOverlap is how far each cycle re-reads behind the previous one's start, so an entry
	// delivered late is still read; duplicates are dropped on insertId. Zero uses
	// DefaultReorderingWindow.
	AuditOverlap time.Duration
	// AuditBudget bounds one window read. Zero values use the published defaults (auditlog.go).
	AuditBudget AuditReadBudget
	// IncidentWindow is how far back each cycle reads alerting incidents. Zero uses one interval
	// plus the overlap.
	IncidentWindow time.Duration
	// Now and Wait are the clock and the sleep between cycles. Injectable for tests.
	Now  func() time.Time
	Wait func(context.Context, time.Duration) error
	// Logger is optional.
	Logger *slog.Logger
}

// Poller is the live [feeder.Source].
type Poller struct {
	opts PollerOptions
	log  *slog.Logger

	transport *Transport
	queue     []feeder.Payload
	cycles    int
	// auditSince is where the next audit window starts. Zero before the first cycle.
	auditSince time.Time
	// flagsRead is whether the Cloud SQL flag catalogue has been read: it is global, not per project,
	// and does not change between cycles, so it is read once per run.
	flagsRead bool
}

var _ feeder.Source = (*Poller)(nil)

// NewPoller validates the options.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Connect == nil {
		return nil, errors.New("gcp: a poller with no transport to connect")
	}
	if len(opts.Scope.Projects) == 0 || len(opts.Scope.Regions) == 0 {
		return nil, errors.New("gcp: a poller needs projects and regions; a scope of none reads " +
			"nothing and would report a quiet window (FR-131)")
	}
	for name, d := range map[string]time.Duration{
		"interval": opts.Interval, "audit lookback": opts.AuditLookback,
		"audit overlap": opts.AuditOverlap, "incident window": opts.IncidentWindow,
	} {
		if d < 0 {
			return nil, fmt.Errorf("gcp: a negative %s (%s)", name, d)
		}
	}
	if opts.Cycles < 0 {
		return nil, fmt.Errorf("gcp: a negative cycle count (%d)", opts.Cycles)
	}
	if opts.Interval == 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.AuditLookback == 0 {
		opts.AuditLookback = DefaultAuditLookback
	}
	if opts.AuditOverlap == 0 {
		opts.AuditOverlap = DefaultReorderingWindow
	}
	if opts.IncidentWindow == 0 {
		opts.IncidentWindow = opts.Interval + opts.AuditOverlap
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Wait == nil {
		opts.Wait = sleep
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Poller{opts: opts, log: log}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Next returns the next payload, running a cycle when the last one has been consumed.
func (p *Poller) Next(ctx context.Context) (feeder.Payload, error) {
	for len(p.queue) == 0 {
		if p.opts.Cycles > 0 && p.cycles >= p.opts.Cycles {
			return feeder.Payload{}, io.EOF
		}
		if p.cycles > 0 {
			if err := p.opts.Wait(ctx, p.opts.Interval); err != nil {
				return feeder.Payload{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return feeder.Payload{}, err
		}
		if p.transport == nil {
			transport, err := p.opts.Connect(ctx)
			if err != nil {
				return feeder.Payload{}, fmt.Errorf("gcp: connecting the area readers: %w", err)
			}
			if transport == nil {
				return feeder.Payload{}, errors.New("gcp: connecting the area readers returned none")
			}
			p.transport = transport
		}
		if err := p.cycle(ctx); err != nil {
			return feeder.Payload{}, err
		}
	}
	next := p.queue[0]
	p.queue = p.queue[1:]
	return next, nil
}

// cycleState is one cycle's working set.
type cycleState struct {
	poller  *Poller
	ctx     context.Context
	at      time.Time
	reasons []string
	quota   bool
	covered []string
}

// failed records a failed read as a reason the cycle is partial. It returns an error only for what
// must end the run: a cancelled context.
func (c *cycleState) failed(what string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, yielded := gcpx.Yielded(err); yielded {
		c.quota = true
	}
	c.reasons = append(c.reasons, fmt.Sprintf("%s: %v", what, err))
	c.poller.log.WarnContext(c.ctx, "gcp read failed; the cycle is partial", "read", what, "error", err)
	return nil
}

func (c *cycleState) push(kind string, raw []byte) {
	c.poller.queue = append(c.poller.queue, feeder.Payload{Kind: kind, At: c.at, Bytes: raw})
}

func (c *cycleState) pushJSON(kind string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("gcp: encoding a %s payload: %w", kind, err)
	}
	c.push(kind, raw)
	return nil
}

// pushProtos writes a list of messages as `{"<field>":[…]}`, each element as the API's own JSON.
func (c *cycleState) pushProtos(kind, field string, items []proto.Message) error {
	var buf bytes.Buffer
	buf.WriteString(`{"` + field + `":[`)
	for i, item := range items {
		raw, err := protojson.Marshal(item)
		if err != nil {
			return fmt.Errorf("gcp: encoding a %s payload: %w", kind, err)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteString(`]}`)
	c.push(kind, buf.Bytes())
	return nil
}

func protos[T proto.Message](items []T) []proto.Message {
	out := make([]proto.Message, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// maxPacedWaits bounds how many times one read waits out the budget's pacing before the cycle calls
// it a quota stop. Pacing is a delay, not a refusal (gcpx.YieldPaced), so it is waited out; but a read
// paced over and over is a cycle spending its interval waiting, and saying so is better.
const maxPacedWaits = 5

// paced runs one metered read, waiting out a pacing yield until the budget allows it. A reserve
// yield, or pacing past maxPacedWaits, is returned for the cycle to report as quota_exhausted.
func paced[T any](c *cycleState, read func() (T, error)) (T, error) {
	for waits := 0; ; waits++ {
		out, err := read()
		y, yielded := gcpx.Yielded(err)
		if !yielded || y.Kind != gcpx.YieldPaced || waits >= maxPacedWaits {
			return out, err
		}
		wait := y.RetryAt.Sub(c.poller.opts.Now())
		if wait > 0 {
			if err := c.poller.opts.Wait(c.ctx, wait); err != nil {
				return out, err
			}
		}
	}
}

// cycle reads every area once, in the order the feeder needs them: configuration before the topology
// that references it, the audit stream after both so a specialised path can claim its entries, and
// the marker last.
func (p *Poller) cycle(ctx context.Context) error {
	p.cycles++
	start := p.opts.Now().UTC()
	c := &cycleState{poller: p, ctx: ctx, at: start}
	t := p.transport
	scope := p.opts.Scope

	if t.SQL != nil {
		if err := p.readSQL(c, t.SQL, scope.Projects); err != nil {
			return err
		}
	}
	if t.GKE != nil {
		for _, project := range scope.Projects {
			clusters, err := paced(c, func() ([]*container.Cluster, error) { return t.GKE.ListClusters(ctx, project) })
			if err := c.failed("container.clusters.list "+project, err); err != nil {
				return err
			}
			if err == nil {
				if err := c.pushJSON(PayloadGKEClusters, container.ListClustersResponse{Clusters: clusters}); err != nil {
					return err
				}
			}
		}
	}
	if t.Run != nil {
		for _, project := range scope.Projects {
			for _, region := range scope.Regions {
				if err := p.readRun(c, t.Run, project, region); err != nil {
					return err
				}
			}
		}
	}
	if t.Monitoring != nil {
		for _, project := range scope.Projects {
			policies, err := paced(c, func() ([]*monitoringpb.AlertPolicy, error) {
				return t.Monitoring.ListAlertPolicies(ctx, project)
			})
			if err := c.failed("monitoring.alertPolicies.list "+project, err); err != nil {
				return err
			}
			if err == nil {
				if err := c.pushProtos(PayloadAlertPolicies, "alertPolicies", protos(policies)); err != nil {
					return err
				}
			}
		}
	}
	if raw, ok := t.AlertIncidents.(rawAlertLister); ok && t.AlertIncidents != nil {
		for _, project := range scope.Projects {
			alerts, err := paced(c, func() ([]json.RawMessage, error) {
				return raw.ListAlertsJSON(ctx, project, start.Add(-p.opts.IncidentWindow), start)
			})
			if err := c.failed("monitoring.alerts.list "+project, err); err != nil {
				return err
			}
			if err == nil {
				if err := c.pushJSON(PayloadAlerts, map[string]any{"alerts": alerts}); err != nil {
					return err
				}
			}
		}
	}
	if t.AuditLogs != nil {
		if err := p.readAudit(c, t.AuditLogs, scope.Projects); err != nil {
			return err
		}
	}

	marker := PollMarker{
		Outcome:  "complete",
		Covered:  c.covered,
		Projects: scope.Projects,
		Regions:  scope.Regions,
	}
	if len(c.reasons) > 0 {
		marker.Outcome = "partial"
		marker.Reason = strings.Join(c.reasons, "; ")
	}
	if c.quota {
		marker.StopReason = string(gcpx.StopQuota)
	}
	return c.pushJSON(PayloadPollMarker, marker)
}

// readRun reads one (project, region): its services, then every service's revisions in one call.
// The pair is covered only when both reads succeeded — a services list without its revisions is a
// topology whose traffic splits name revisions nothing describes.
func (p *Poller) readRun(c *cycleState, run CloudRunReader, project, region string) error {
	services, err := paced(c, func() ([]*runpb.Service, error) { return run.ListServices(c.ctx, project, region) })
	if err := c.failed("run.services.list "+project+"/"+region, err); err != nil {
		return err
	}
	if err != nil {
		return nil
	}
	revisions, revErr := paced(c, func() ([]*runpb.Revision, error) {
		return run.ListRevisions(c.ctx, project, region, "-")
	})
	if err := c.failed("run.revisions.list "+project+"/"+region, revErr); err != nil {
		return err
	}
	if err := c.pushProtos(PayloadServices, "services", protos(services)); err != nil {
		return err
	}
	if revErr != nil {
		return nil
	}
	if err := c.pushProtos(PayloadRevisions, "revisions", protos(revisions)); err != nil {
		return err
	}
	c.covered = append(c.covered, project+"/"+region)
	return nil
}

// readSQL reads the flag catalogue once per run, then each project's instances and each instance's
// operation history.
func (p *Poller) readSQL(c *cycleState, sql CloudSQLReader, projects []string) error {
	if !p.flagsRead {
		flags, err := paced(c, func() ([]*sqladmin.Flag, error) { return sql.ListFlags(c.ctx) })
		if err := c.failed("cloudsql.flags.list", err); err != nil {
			return err
		}
		if err == nil {
			if err := c.pushJSON(PayloadSQLFlags, sqladmin.FlagsListResponse{Items: flags}); err != nil {
				return err
			}
			p.flagsRead = true
		}
	}
	for _, project := range projects {
		instances, err := paced(c, func() ([]*sqladmin.DatabaseInstance, error) { return sql.ListInstances(c.ctx, project) })
		if err := c.failed("cloudsql.instances.list "+project, err); err != nil {
			return err
		}
		if err != nil {
			continue
		}
		if err := c.pushJSON(PayloadSQLInstances, sqladmin.InstancesListResponse{Items: instances}); err != nil {
			return err
		}
		names := make([]string, 0, len(instances))
		for _, instance := range instances {
			if instance != nil && instance.Name != "" {
				names = append(names, instance.Name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			operations, err := paced(c, func() ([]*sqladmin.Operation, error) {
				return sql.ListOperations(c.ctx, project, name)
			})
			if err := c.failed("cloudsql.operations.list "+project+"/"+name, err); err != nil {
				return err
			}
			if err == nil {
				if err := c.pushJSON(PayloadSQLOperations, sqladmin.OperationsListResponse{Items: operations}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// AuditLogFilter is the request filter for one project's admin-activity window (FR-040). It is
// published because the checkpoint records it: a reader can see what was asked for.
func AuditLogFilter(project string, since time.Time) string {
	return fmt.Sprintf(`logName="projects/%s/logs/cloudaudit.googleapis.com%s" AND timestamp>="%s"`,
		project, AdminActivityLogSuffix, since.UTC().Format(time.RFC3339))
}

// readAudit reads each project's admin-activity entries since the window start, one payload per
// page, bounded by the published window budget. The window advances only when every project's read
// finished: a window cut short is re-read from the same start next cycle, and the overlap's duplicates
// are dropped on insertId.
func (p *Poller) readAudit(c *cycleState, logs AuditLogReader, projects []string) error {
	since := p.auditSince
	if since.IsZero() {
		since = c.at.Add(-p.opts.AuditLookback)
	}
	finished := true
	for _, project := range projects {
		done, err := p.readAuditWindow(c, logs, project, since)
		if err != nil {
			return err
		}
		finished = finished && done
	}
	if finished {
		p.auditSince = c.at.Add(-p.opts.AuditOverlap)
	} else {
		p.auditSince = since
	}
	return nil
}

func (p *Poller) readAuditWindow(c *cycleState, logs AuditLogReader, project string, since time.Time) (bool, error) {
	budget := p.opts.AuditBudget
	started := p.opts.Now()
	token := ""
	for pages := 0; ; pages++ {
		if pages >= budget.pages() {
			return false, c.failed("logging.entries.list "+project, errors.New(AuditStopPages))
		}
		if p.opts.Now().Sub(started) > budget.wall() {
			return false, c.failed("logging.entries.list "+project, errors.New(AuditStopWall))
		}
		type page struct {
			entries []*loggingpb.LogEntry
			next    string
		}
		got, err := paced(c, func() (page, error) {
			entries, next, err := logs.ListEntries(c.ctx, &loggingpb.ListLogEntriesRequest{
				ResourceNames: []string{"projects/" + project},
				Filter:        AuditLogFilter(project, since),
				OrderBy:       "timestamp asc",
				PageToken:     token,
			})
			return page{entries, next}, err
		})
		entries, next := got.entries, got.next
		if err != nil {
			return false, c.failed("logging.entries.list "+project, err)
		}
		if err := c.pushAuditPage(entries); err != nil {
			return false, err
		}
		// An empty page with a token is not the end: GCP returns those while it is still searching,
		// and the token is the only thing that says the search is over (auditlog.go).
		if next == "" {
			return true, nil
		}
		token = next
	}
}

// pushAuditPage writes one page of audit entries as the API's JSON.
//
// Two fields of an AuditLog are cleared first, because each holds an `Any` of a type nobody can
// promise is linked into this binary, and protojson refuses an entry whose Any it cannot resolve: the
// first live run met `google.iam.admin.v1.AuditData` in `serviceData` on its first IAM entry. Neither is
// read by the feeder. `serviceData` is deprecated in favour of `metadata`, and is per-service; the
// details of a `status` are free-form error payloads, where only the code and message say whether the
// operation succeeded. An entry that still cannot be encoded is skipped and named, so the cycle says it
// is partial rather than ending the run or going quiet.
func (c *cycleState) pushAuditPage(entries []*loggingpb.LogEntry) error {
	var buf bytes.Buffer
	buf.WriteString(`{"entries":[`)
	written := 0
	for _, entry := range entries {
		raw, err := protojson.Marshal(withoutOpaqueAuditFields(entry))
		if err != nil {
			if err := c.failed("encoding audit entry "+entry.GetInsertId(), err); err != nil {
				return err
			}
			continue
		}
		if written > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		written++
	}
	buf.WriteString(`]}`)
	c.push(PayloadAuditEntries, buf.Bytes())
	return nil
}

// withoutOpaqueAuditFields returns the entry with its AuditLog's `serviceData` and `status.details`
// cleared, or the entry unchanged when its payload is not an AuditLog this binary can read.
func withoutOpaqueAuditFields(entry *loggingpb.LogEntry) *loggingpb.LogEntry {
	payload := entry.GetProtoPayload()
	if payload == nil {
		return entry
	}
	var log audit.AuditLog
	if err := payload.UnmarshalTo(&log); err != nil {
		return entry
	}
	log.ServiceData = nil //nolint:staticcheck // deprecated by Google, and cleared precisely because nothing reads it
	if log.Status != nil {
		log.Status.Details = nil
	}
	repacked, err := anypb.New(&log)
	if err != nil {
		return entry
	}
	clone := proto.Clone(entry).(*loggingpb.LogEntry)
	clone.Payload = &loggingpb.LogEntry_ProtoPayload{ProtoPayload: repacked}
	return clone
}

// rawAlertLister is the incident reader's raw form: the API's own JSON per incident, which is what the
// `alerts` payload carries and what the feeder's one decoder reads, live and replayed alike.
type rawAlertLister interface {
	ListAlertsJSON(ctx context.Context, project string, start, end time.Time) ([]json.RawMessage, error)
}
