// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The worker call path (T063, T068, T090, T091 seam; FR-018a, FR-021, FR-027, FR-047a).
//
// One function issues every worker call, and everything that has to happen around a call happens
// there: admission before it is issued, the request recorded before it leaves, the response
// recorded when it returns, the evidence item built from it, the pointers and handles it minted
// filed in the catalogue, the injection detector run over the two fields that carry a source's own
// prose, and the tool result rendered.
//
// The ordering is load-bearing in two places. Admission comes first, so a call that would breach a
// budget is never issued (FR-047a). And the request is recorded *before* the call rather than
// after it, so that a call that hung or crashed the process still left a trace of having been
// attempted — a recording that only holds successful calls is a recording of a different run.

// Workers is the set of workers this investigation may call, with the capability index the engine
// resolves a tool name through.
type Workers struct {
	caller   *worker.Caller
	registry *worker.Registry

	byCapability map[string][]string
	backendOf    map[string]string
	costClassOf  map[string]map[string]worker.CostClass
}

// NewWorkers indexes a registry by capability.
//
// More than one worker may declare a term — `compare` is declared by metrics and by traces,
// because a latency comparison and an error-rate comparison are the same question asked of
// different sources. Where that happens the **pointer decides**: a metric pointer routes to the
// metrics worker, a log pointer to logs, a trace pointer to traces. That rule is published rather
// than implicit, because an answer whose provenance cannot be established is not evidence
// (FR-016), and "whichever worker registered first" is not a provenance.
func NewWorkers(registry *worker.Registry, retry worker.RetryPolicy) (*Workers, error) {
	w := &Workers{
		caller:       worker.NewCaller(registry, retry),
		registry:     registry,
		byCapability: map[string][]string{},
		backendOf:    map[string]string{},
		costClassOf:  map[string]map[string]worker.CostClass{},
	}
	for _, description := range registry.Descriptions() {
		w.backendOf[description.Name] = description.SourceOfTruth
		for _, capability := range description.Capabilities {
			w.byCapability[capability.Name] = append(w.byCapability[capability.Name], description.Name)
			if w.costClassOf[description.Name] == nil {
				w.costClassOf[description.Name] = map[string]worker.CostClass{}
			}
			w.costClassOf[description.Name][capability.Name] = capability.CostClass
		}
	}
	for capability := range w.byCapability {
		sort.Strings(w.byCapability[capability])
	}
	return w, nil
}

// For returns the worker that answers a capability when exactly one does, which is every term but
// `compare` in this feature's worker set.
func (w *Workers) For(capability string) (string, bool) {
	names := w.byCapability[capability]
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// The published worker names the pointer-kind routing rule resolves to.
const (
	workerMetrics = "metrics"
	workerLogs    = "logs"
	workerTraces  = "traces"
)

// route picks the worker for one request, applying the published pointer-kind rule where more than
// one worker declares the term.
func (w *Workers) route(capability string, term *investigationv1.AlgebraTerm) (string, bool) {
	names := w.byCapability[capability]
	switch len(names) {
	case 0:
		return "", false
	case 1:
		return names[0], true
	}
	preferred := workerForPointerKind(pointerKindOf(term))
	for _, name := range names {
		if name == preferred {
			return name, true
		}
	}
	return names[0], true
}

// pointerKindOf reads the pointer a term carries, where it carries one.
func pointerKindOf(term *investigationv1.AlgebraTerm) graphv1.PointerKind {
	switch body := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return body.Compare.GetPointer().GetKind()
	case *investigationv1.AlgebraTerm_Onset:
		return body.Onset.GetPointer().GetKind()
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return body.NewLogPatterns.GetPointer().GetKind()
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return body.ErrorsByVersion.GetPointer().GetKind()
	case *investigationv1.AlgebraTerm_MonitorState:
		return body.MonitorState.GetPointer().GetKind()
	default:
		return graphv1.PointerKind_POINTER_KIND_UNSPECIFIED
	}
}

func workerForPointerKind(kind graphv1.PointerKind) string {
	switch kind {
	case graphv1.PointerKind_LOG:
		return workerLogs
	case graphv1.PointerKind_TRACE:
		return workerTraces
	default:
		return workerMetrics
	}
}

// Capabilities returns every declared capability, sorted.
func (w *Workers) Capabilities() []string {
	out := make([]string, 0, len(w.byCapability))
	for capability := range w.byCapability {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// Backend returns the source of truth a worker speaks for, which is what a per-backend budget and
// a quota share are measured against.
func (w *Workers) Backend(workerName string) string { return w.backendOf[workerName] }

// Answer is one worker call as the engine holds it: the request, the response, the evidence it
// became, and the text the model will see.
type Answer struct {
	// Worker and Capability are the call.
	Worker     string
	Capability string
	// Request and Response are the published algebra messages, recorded verbatim.
	Request  *investigationv1.AlgebraRequest
	Response *investigationv1.AlgebraResponse
	// Graph is the graph family's own response message, set only for a graph term.
	Graph proto.Message
	// Record is the per-attempt call record.
	Record worker.CallRecord
	// EvidenceID is the ledger evidence item this answer became.
	EvidenceID string
	// Item and Evidence are the two views of that item: the ledger's, which carries what a
	// posterior needs, and the checker's, which carries the digest body.
	Item     ledger.EvidenceItem
	Evidence verify.Evidence
	// ToolResult is the text of the `tool_result` block. It is the only channel from a worker to
	// the model (FR-017).
	ToolResult string
	// Injections are the attempts the detector saw in the free-text and exemplar fields. Each is
	// recorded as its own evidence item and changes nothing else.
	Injections []Injection
	// Minted names the pointer and handle ids this answer added to the catalogue.
	Minted []string
	// Refused is set when admission refused the call. The call was not issued.
	Refused *budget.Decision

	// link and linkAbsent are carried between issue and record.
	link       string
	linkAbsent string
}

// DeepLinker turns an algebra request into a link a person can follow. A deployment with no
// console has none, and FR-057d requires the absence to be explained rather than silent.
type DeepLinker interface {
	// Link returns the deep link and, when there is none, the published reason there is none.
	Link(req *investigationv1.AlgebraRequest) (link string, absentReason string)
}

// NoDeepLinks is the DeepLinker a recorded backend uses: there is no console behind a recording,
// and saying so is better than an empty field.
type NoDeepLinks struct{}

// Link implements DeepLinker.
func (NoDeepLinks) Link(*investigationv1.AlgebraRequest) (string, string) {
	return "", "the answer came from a recorded world, which has no console to link to"
}

// Call issues one worker call and records what came back.
//
// It is `issue` followed by `record`, and the two are separable for one reason: the first wave
// runs its calls in parallel, and an evidence id handed out inside a goroutine would depend on
// scheduling. Gathering is parallel; the ledger writes that follow it are sequential and ordered,
// so two runs over the same world produce the same evidence ids and the same trajectory (FR-013a).
func (e *Engine) Call(ctx context.Context, req *investigationv1.AlgebraRequest) (*Answer, error) {
	answer, err := e.issue(ctx, req)
	if err != nil || answer.Refused != nil {
		return answer, err
	}
	if err := e.record(answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// admitted is one call that has passed admission and is not yet issued: everything the issuing
// needs, resolved once, plus the reservation the budget booked for it.
//
// It exists so that admission can be lifted out of the goroutine that issues the call. Admission
// mutates a shared budget, so the order admissions are made in decides which call loses when a cap
// binds; making them inside the wave's goroutines made that order a function of the Go scheduler
// (firstwave.go).
type admitted struct {
	req        *investigationv1.AlgebraRequest
	worker     string
	capability string
	backend    string
	costClass  string
	width      time.Duration
	link       string
	linkAbsent string
	// reservation is what Admit booked. It is reconciled by RecordWorkerCall and released if the
	// call is never made.
	reservation *budget.Reservation
}

// preAdmit resolves a request to a worker and passes it through admission, booking the call.
//
// It returns exactly one of: an admitted call, a refused Answer carrying the typed decision, or an
// error. It touches the shared budget and nothing else the engine holds, so it is called
// sequentially — by `issue` for a single call, and by the first wave for the whole wave, in the
// wave's plan order, before any of it is issued.
func (e *Engine) preAdmit(req *investigationv1.AlgebraRequest) (*admitted, *Answer, error) {
	capability := backend.TermName(req.GetTerm())
	if capability == "" {
		return nil, nil, backend.OutsideAlgebra("")
	}
	workerName, ok := e.workers.route(capability, req.GetTerm())
	if !ok {
		return nil, nil, fmt.Errorf("engine: no registered worker declares %s; the registered capabilities are %s",
			capability, strings.Join(e.workers.Capabilities(), ", "))
	}
	backendName := e.workers.Backend(workerName)
	costClass := sdk.CostClassName(e.workers.costClassOf[workerName][capability])
	width := windowWidth(req.GetTerm())
	link, absent := e.links.Link(req)

	decision := e.budget.Admit(budget.Request{
		Kind:               budget.KindWorker,
		Worker:             workerName,
		Backend:            backendName,
		CostClass:          costClass,
		WindowWidth:        width,
		ServesHypothesisID: req.GetServesHypothesisId(),
		Question:           req.GetDiscriminatingQuestion(),
		DeepLink:           link,
	})
	if !decision.Admitted {
		return nil, &Answer{
			Worker: workerName, Capability: capability, Request: req, Refused: &decision,
		}, nil
	}
	return &admitted{
		req: req, worker: workerName, capability: capability, backend: backendName,
		costClass: costClass, width: width, link: link, linkAbsent: absent,
		reservation: decision.Reservation,
	}, nil, nil
}

// issueAdmitted records the request, calls the worker and records the response. It assigns no
// evidence id and writes nothing to the ledger, so it is safe to run concurrently.
//
// A call that does not leave releases its reservation, so the headroom admission booked for it
// returns to the investigation rather than being spent by a call that never happened.
func (e *Engine) issueAdmitted(ctx context.Context, adm *admitted) (*Answer, error) {
	return e.issueAdmittedInto(ctx, e.trajectory, adm)
}

// issueAdmittedInto is issueAdmitted with the trajectory the two records go to named explicitly,
// so a caller issuing calls in parallel can give each branch a trajectory of its own and splice
// them back in its own order (`RunFirstWave`). Passing the engine's own trajectory is the
// sequential case and is what `issueAdmitted` does.
func (e *Engine) issueAdmittedInto(ctx context.Context, trajectory *Trajectory, adm *admitted) (*Answer, error) {
	issued := false
	defer func() {
		if !issued {
			adm.reservation.Release()
		}
	}()

	termKey, err := backend.TermKey(adm.req.GetTerm())
	if err != nil {
		return nil, fmt.Errorf("engine: term key: %w", err)
	}
	trajectory.WorkerRequest(adm.worker, adm.req, termKey)

	resp, record, err := e.workers.caller.Call(ctx, adm.worker, worker.Request{
		Capability: adm.capability,
		Mode:       e.mode,
		Algebra:    adm.req,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: call %s/%s: %w", adm.worker, adm.capability, err)
	}
	trajectory.WorkerResponse(resp.Algebra)

	coverage := resp.Algebra.GetDigest().GetCoverage()
	issued = true
	e.budget.RecordWorkerCall(budget.WorkerCall{
		Worker:            adm.worker,
		Backend:           adm.backend,
		CostClass:         adm.costClass,
		WindowWidth:       adm.width,
		RemainingQuota:    coverage.GetRemainingQuota(),
		QuotaWindow:       time.Duration(coverage.GetQuotaWindowSeconds()) * time.Second,
		QuotaUndetermined: coverage.GetQuotaUndetermined() || coverage.GetRemainingQuota() == 0,
		Reservation:       adm.reservation,
	})

	return &Answer{
		Worker:     adm.worker,
		Capability: adm.capability,
		Request:    adm.req,
		Response:   resp.Algebra,
		Graph:      resp.Graph,
		Record:     record,
		link:       adm.link,
		linkAbsent: adm.linkAbsent,
	}, nil
}

// issue is admission followed immediately by the call, for the callers that issue one call at a
// time. The first wave takes the two halves apart.
func (e *Engine) issue(ctx context.Context, req *investigationv1.AlgebraRequest) (*Answer, error) {
	adm, refused, err := e.preAdmit(req)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return refused, nil
	}
	return e.issueAdmitted(ctx, adm)
}

// record turns an issued answer into an evidence item, mints the pointers and handles it carried,
// runs the injection detector and renders the tool result. It is sequential by construction.
func (e *Engine) record(answer *Answer) error {
	if answer.EvidenceID != "" {
		return nil
	}
	item, evidence, err := e.recordEvidence(answer, answer.Worker, answer.Capability, answer.link, answer.linkAbsent)
	if err != nil {
		return err
	}
	answer.EvidenceID = item.ID
	answer.Item = item
	answer.Evidence = evidence

	e.note(answer)
	answer.Minted = e.mint(answer)
	answer.Injections = DetectInjections(answer.Response)
	if err := e.recordInjections(answer); err != nil {
		return err
	}

	extra := ""
	if len(answer.Minted) > 0 {
		extra = "catalogued: " + strings.Join(answer.Minted, ", ") + "\n"
	}
	extra += "evidence_id: " + item.ID + "\n"
	if graphText := renderGraph(answer.Graph, e.catalogue); graphText != "" {
		extra += graphText
	}
	text, err := ToolResult(answer.Response, extra)
	if err != nil {
		return err
	}
	answer.ToolResult = text
	e.remember(answer)
	return nil
}

// recordEvidence builds the ledger's evidence item and the checker's view of the same answer, and
// files the ledger's.
func (e *Engine) recordEvidence(answer *Answer, workerName, capability, link, absent string) (ledger.EvidenceItem, verify.Evidence, error) {
	resp := answer.Response
	description, _ := e.workers.registry.Lookup(workerName)
	sourceOfTruth := ""
	if description != nil {
		sourceOfTruth = description.Describe().SourceOfTruth
	}

	digest := resp.GetDigest()
	if link == "" && digest.GetDeepLink() != "" {
		link = digest.GetDeepLink()
		absent = ""
	}

	item := ledger.EvidenceItem{
		ID:                   e.nextEvidenceID(),
		Kind:                 evidenceKind(capability),
		Worker:               workerName,
		Capability:           capability,
		SourceOfTruth:        sourceOfTruth,
		Term:                 answer.Request.GetTerm(),
		ValidAt:              answer.Request.GetValidAt().AsTime(),
		ObservedAt:           answer.Request.GetObservedAt().AsTime(),
		CalledAt:             e.now(),
		Mode:                 string(e.mode),
		Outcome:              outcomeName(resp.GetOutcome()),
		ResponseDigest:       resp.GetResponseDigest(),
		ResponseKey:          resp.GetTermKey(),
		Coverage:             digest.GetCoverage(),
		DeepLink:             link,
		DeepLinkAbsentReason: absent,
		Truncated:            digest.GetTruncation().GetTruncated(),
		TruncationNote:       digest.GetTruncation().GetWhatWasDropped(),
		FreeText:             digest.GetFreeText(),
	}
	if item.Coverage == nil {
		// A response with no coverage block would be refused by the ledger, and rightly: a
		// digest with no coverage is a claim about an unknown amount of data. A failure carries
		// its own minimal coverage so the failure is still recordable as evidence (FR-027).
		item.Coverage = &investigationv1.Coverage{
			DataSource: sourceOfTruth,
			Sampling:   "none: the worker did not answer, so nothing was searched",
		}
	}
	if item.Truncated && item.TruncationNote == "" {
		item.TruncationNote = digest.GetTruncation().GetCriterion()
	}
	if err := e.ledger.AddEvidence(item); err != nil {
		return ledger.EvidenceItem{}, verify.Evidence{}, fmt.Errorf("engine: record evidence: %w", err)
	}
	return item, verify.EvidenceFromLedger(item, digest), nil
}

// recordInjections files one evidence item per detected attempt. The investigation is otherwise
// untouched: same scope, same budgets, same worker set, same posture, same output (FR-017).
func (e *Engine) recordInjections(answer *Answer) error {
	for _, injection := range answer.Injections {
		item := ledger.EvidenceItem{
			ID:                   e.nextEvidenceID(),
			Kind:                 EvidenceKindInjectionAttempt,
			Worker:               answer.Worker,
			Capability:           answer.Capability,
			Term:                 answer.Request.GetTerm(),
			ValidAt:              answer.Request.GetValidAt().AsTime(),
			ObservedAt:           answer.Request.GetObservedAt().AsTime(),
			CalledAt:             e.now(),
			Mode:                 string(e.mode),
			Outcome:              outcomeName(answer.Response.GetOutcome()),
			ResponseDigest:       answer.Response.GetResponseDigest(),
			ResponseKey:          answer.Response.GetTermKey(),
			Coverage:             answer.Response.GetDigest().GetCoverage(),
			DeepLinkAbsentReason: "a detected injection attempt is an observation about the answer, not a query",
			FreeText:             injection.Statement(),
		}
		if item.Coverage == nil {
			item.Coverage = &investigationv1.Coverage{DataSource: answer.Worker}
		}
		if err := e.ledger.AddEvidence(item); err != nil {
			return fmt.Errorf("engine: record injection attempt: %w", err)
		}
		e.injections = append(e.injections, item.ID)
	}
	return nil
}

// InjectionEvidenceIDs returns the evidence items recorded for detected injection attempts.
func (e *Engine) InjectionEvidenceIDs() []string {
	return append([]string(nil), e.injections...)
}

// mint files the pointers and handles an answer produced, so the model can refer to them without
// ever composing one.
// note files what a graph answer says about identity: which canonical entity id each node has,
// which references the graph publishes for it, and which edges the neighbourhood holds with what
// type.
//
// It exists because two vocabularies meet in every investigation and neither is optional. A
// change's targets and an edge's ends are **canonical entity ids**; a `pointers`, `subgraph` or
// `impact` read takes a **Ref**. Both are the graph's own, and the node versions the graph
// returns carry them together — so the translation is read out of the answers rather than
// invented, and it exists only for an entity some answer actually described.
func (e *Engine) note(answer *Answer) {
	switch graph := answer.Graph.(type) {
	case *graphv1.PointersResponse:
		// The request carried the reference and the answer carries the id, which is the one place
		// the pair is known without an alias list.
		e.catalogue.NoteRef(
			RefString(answer.Request.GetTerm().GetGraph().GetPointers().GetFocus()),
			graph.GetNode().GetEntityId())
		e.catalogue.NoteNode(graph.GetNode())
	case *graphv1.SubgraphResponse:
		e.catalogue.NoteNode(graph.GetFocus())
		for _, node := range graph.GetNodes() {
			e.catalogue.NoteNode(node)
		}
		for _, edge := range graph.GetEdges() {
			e.catalogue.NoteEdge(edge)
		}
	case *graphv1.DiffResponse:
		for _, node := range graph.GetNodesAdded() {
			e.catalogue.NoteNode(node)
		}
		for _, node := range graph.GetNodesRemoved() {
			e.catalogue.NoteNode(node)
		}
		for _, delta := range graph.GetNodesChanged() {
			e.catalogue.NoteNode(delta.GetAfter())
			e.catalogue.NoteNode(delta.GetBefore())
		}
		for _, change := range graph.GetChanges() {
			e.catalogue.NoteNode(change.GetChange())
		}
		// The targets the changes landed on, which are usually unchanged and so in no delta above:
		// without them the first wave tests a change on a dependency against the subject (T156).
		for _, node := range graph.GetChangeTargets() {
			e.catalogue.NoteNode(node)
		}
	case *graphv1.ImpactResponse:
		for _, item := range graph.GetDownstream() {
			e.catalogue.NoteNode(item.GetNode())
		}
		for _, item := range graph.GetUpstream() {
			e.catalogue.NoteNode(item.GetNode())
		}
	}
}

func (e *Engine) mint(answer *Answer) []string {
	var out []string
	if pointers, ok := answer.Graph.(*graphv1.PointersResponse); ok {
		entity := RefString(answer.Request.GetTerm().GetGraph().GetPointers().GetFocus())
		kinds := make([]string, 0, len(pointers.GetByKind()))
		for kind := range pointers.GetByKind() {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			for _, pointer := range pointers.GetByKind()[kind].GetPointers() {
				if id := e.catalogue.AddPointer(entity, pointer); id != "" {
					out = append(out, id)
				}
			}
		}
	}
	for _, handle := range handlesOf(answer.Response.GetDigest()) {
		if id := e.catalogue.AddHandle(handle); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// handlesOf collects every drill-down handle a digest carries, in a fixed order.
func handlesOf(digest *investigationv1.Digest) []*investigationv1.Handle {
	if digest == nil {
		return nil
	}
	var out []*investigationv1.Handle
	add := func(d *investigationv1.DrillDown) {
		if h := d.GetHandle(); h != nil && h.GetValue() != "" {
			out = append(out, h)
		}
	}
	switch body := digest.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, series := range body.Metric.GetSeries() {
			add(series.GetDrillDown())
		}
	case *investigationv1.Digest_Log:
		for _, pattern := range body.Log.GetPatterns() {
			add(pattern.GetDrillDown())
		}
	case *investigationv1.Digest_Trace:
		for _, group := range body.Trace.GetGroups() {
			add(group.GetDrillDown())
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, version := range body.ErrorsByVersion.GetVersions() {
			add(version.GetDrillDown())
		}
	}
	return out
}

// renderGraph adds the part of a graph answer the model needs that a digest cannot carry: the
// pointer ids it just earned, and the ranked candidates a diff produced with their actor kind and
// signed distance.
func renderGraph(message proto.Message, cat *Catalogue) string {
	switch answer := message.(type) {
	case *graphv1.PointersResponse:
		return "pointers:\n" + cat.Render()
	case *graphv1.DiffResponse:
		var b strings.Builder
		fmt.Fprintf(&b, "ranked candidates (ranking formula: %s — the graph's order, never re-ranked):\n",
			answer.GetRankingFormula())
		for i, change := range answer.GetChanges() {
			fmt.Fprintf(&b, "  %d. %s  score=%.6f  hops=%d  signed_distance=%ds  post_reference=%t  actor_kind=%s",
				i+1, change.GetChange().GetEntityId(), change.GetScore(), change.GetHopDistance(),
				change.GetSignedTimeDistanceSeconds(), change.GetPostReference(), change.GetActorKind())
			// What the change is and what it landed on, by the references the graph publishes (T156):
			// an entity id tells the model nothing about which service a candidate touched. Quoted,
			// because an alias is a string a source wrote.
			if ref := cat.RefFor(change.GetChange().GetEntityId()); ref != "" {
				fmt.Fprintf(&b, "  change=%q", ref)
			}
			if targets := targetRefs(cat, change.GetTargetEntityIds()); targets != "" {
				fmt.Fprintf(&b, "  targets=%s", targets)
			}
			// A stated rollback, and what it moved away from: an operator's judgement the model should
			// weigh, and a change it must not propose rolling back again (004 T155).
			// The commit it shipped, where a source stated one (004 T128): what lets the model tie a
			// ranked change to a code change a person can read.
			for _, key := range change.GetCorrelationKeys() {
				if key.GetNamespace() == "deploy.commit_sha" {
					fmt.Fprintf(&b, "  commit=%s", key.GetValue())
				}
			}
			if c := change.GetChange().GetChange(); c.GetRollback() {
				fmt.Fprintf(&b, "  rollback=true rolled_back_from=%q rolled_back_to=%q",
					c.GetRolledBackFrom(), c.GetRolledBackTo())
			}
			b.WriteString("\n")
		}
		return b.String()
	default:
		return ""
	}
}

// targetRefs names a change's targets by reference, each quoted, falling back to the id for one no
// answer described.
func targetRefs(cat *Catalogue, ids []string) string {
	named := make([]string, 0, len(ids))
	for _, id := range ids {
		ref := cat.RefFor(id)
		if ref == "" {
			ref = id
		}
		named = append(named, strconv.Quote(ref))
	}
	return strings.Join(named, ",")
}

// evidenceKind maps a capability onto the published evidence kind (data-model
// §investigation.evidence_items).
func evidenceKind(capability string) string {
	switch capability {
	case "onset":
		return "onset_estimate"
	case "resolution_audit":
		return "resolution_audit"
	case "knowledge_search":
		return "knowledge_item"
	case "subgraph", "diff", "impact", "pointers", "node_history", "extent":
		return "graph_answer"
	default:
		return "algebra_answer"
	}
}

// windowWidth is the widest window the term asks for, which is what the max-window budget and the
// per-call window cap are measured against.
func windowWidth(term *investigationv1.AlgebraTerm) time.Duration {
	widest := time.Duration(0)
	consider := func(w *investigationv1.Window) {
		if w.GetStart() == nil || w.GetEnd() == nil {
			return
		}
		if d := w.GetEnd().AsTime().Sub(w.GetStart().AsTime()); d > widest {
			widest = d
		}
	}
	switch body := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		pair := body.Compare.GetWindows()
		consider(pair.GetBaseline())
		consider(pair.GetSymptom())
		if width := time.Duration(pair.GetWidthSeconds()) * time.Second; width > widest {
			widest = width
		}
	case *investigationv1.AlgebraTerm_Onset:
		consider(body.Onset.GetSearchWindow())
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		consider(body.NewLogPatterns.GetWindow())
		consider(body.NewLogPatterns.GetBaselineWindow())
	case *investigationv1.AlgebraTerm_ErrorSpans:
		consider(body.ErrorSpans.GetWindow())
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		consider(body.ErrorsByVersion.GetWindow())
	case *investigationv1.AlgebraTerm_MonitorState:
		consider(body.MonitorState.GetWindow())
	case *investigationv1.AlgebraTerm_Graph:
		if diff := body.Graph.GetDiff(); diff != nil && diff.GetT1() != nil && diff.GetT2() != nil {
			if d := diff.GetT2().AsTime().Sub(diff.GetT1().AsTime()); d > widest {
				widest = d
			}
		}
	}
	return widest
}
