// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The live digest boundary (T042, FR-110, contract §5 call site 2).
//
// The asymmetry this call site closes is the one worth naming: if sanitisation were only a recording
// step, a live investigation could surface what a recording would not be allowed to keep, and the
// difference between "safe to store" and "safe to see" would be a difference nobody wrote down. So
// people identifiers are dropped in live mode too, and log content is reduced to masked templates
// before it leaves the process — the same table, the same policy version, the same key.
//
// # Ordering, and why it is not a detail
//
// Two redactors act on a digest: this feature's sanitiser and feature 002's Redactor. They must
// agree, because the graph this feature records and the digests this backend answers with are one
// corpus, and the join between them is the whole claim of contract §2.3 property 1.
//
// They agree by ordering. `engine.Redactor.Pseudonymise` returns a value unchanged when it already
// carries the `px_` prefix, so a value this package pseudonymised passes through untouched and one
// token appears on both sides. The reverse does not hold: a value 002 pseudonymised first is an
// untyped `px_` token that this package then leaves alone, so the recorded graph and the digest hold
// different tokens for one service. Sanitise first, always. SanitiseThenRedact is the only ordering
// this package offers, which is how that is enforced rather than remembered.

// Sanitiser is the sanitisation seam this backend applies to a live answer. It is an interface over
// the two calls this file needs rather than the concrete type, so that a test can plant a sanitiser
// that fails and prove the failure reaches the caller.
type Sanitiser interface {
	Policy() *sanitise.Policy
	Template(where, line string) (string, bool, error)
	// Field applies the sanitisation table to one field. It is here because the join keys go
	// through it: the token a digest carries has to be the token the feeder recorded, and that
	// holds only if the same table and the same key compute both.
	Field(where, path, value string) (string, bool, error)
	AssertArtifact(where string, data []byte) error
}

// DeclaredRedactionPolicy is what this backend publishes in its BackendDescription. It is derived
// from the sanitisation table rather than written out beside it, so the declaration cannot drift from
// the dispositions actually applied — a backend that quietly widened what it returns without
// widening what it declared is what `undeclared_redaction` exists to catch, and a hand-maintained
// second list is how that happens.
func DeclaredRedactionPolicy(policy *sanitise.Policy) *investigationv1.RedactionPolicy {
	return &investigationv1.RedactionPolicy{
		DroppedFields: append([]string{
			engine.FieldPeopleIdentifiers,
			engine.FieldMonitorBody,
		}, policy.Fields(sanitise.Dropped)...),
		PseudonymisedFields: append([]string{
			engine.FieldJoinKeyWorkload,
			engine.FieldJoinKeyPodOrHost,
			// `join_keys.version` is declared here and is NOT in 002's default policy, which
			// leaves it verbatim on the reasoning that a deploy revision has to join a digest
			// to a change in the graph (ADR-0005 D4). Against this vendor that reasoning
			// inverts: the sanitisation table pseudonymises `resource.labels.revision_name`,
			// so the graph holds tokens — and a digest carrying the revision in the clear
			// joins to nothing. Declaring it makes 002's own CheckDeclared enforce that it
			// arrives pseudonymised, which is the guard against forgetting.
			engine.FieldJoinKeyVersion,
			engine.FieldJoinKeyTraceIDs,
			engine.FieldJoinKeySpanIDs,
		}, policy.Fields(sanitise.Pseudonym)...),
		LogBodiesAsTemplates: true,
		PolicyVersion:        policy.Version(),
	}
}

// SanitiseThenRedact applies this feature's sanitisation to a live response and then feature 002's
// redactor, in that order, and refuses if either does. It is the only exported way to finish a
// response in this package.
//
// The version check is not ceremony. Two policies that disagree about what a field's disposition is
// produce a corpus in which the same identifier is sometimes a token and sometimes itself, and the
// symptom is a join that works on half the fixtures.
func SanitiseThenRedact(s Sanitiser, r *engine.Redactor, resp *engine.Response) error {
	if s == nil {
		return fmt.Errorf("gcp: a live answer with no sanitiser; FR-110 says a live investigation " +
			"must not be able to surface what a recording would not be allowed to keep")
	}
	if r == nil {
		return fmt.Errorf("gcp: a live answer with no redactor")
	}
	if got, want := r.Policy().GetPolicyVersion(), s.Policy().Version(); got != want {
		return fmt.Errorf("gcp: the declared redaction policy is version %q and the sanitisation "+
			"contract in force is %q; two policies that disagree produce a corpus whose joins work "+
			"on half the fixtures", got, want)
	}

	// Log content first, and through the sanitiser.
	//
	// Feature 002's Apply below *also* masks a log template, so masking is not what this loop adds —
	// a probe that disables this loop still leaves a masked answer, and it is worth saying so rather
	// than implying otherwise. What only this loop does is apply the *sanitisation contract* to live
	// log content: a pattern still naming a person after masking is dropped from the answer, and a
	// seeded canary in a live template is a refusal. Neither is something 002's redactor knows about,
	// and both are what FR-110 means by a live investigation not surfacing what a recording could not
	// keep.
	if log := resp.GetDigest().GetLog(); log != nil {
		kept := log.GetPatterns()[:0]
		for _, pattern := range log.GetPatterns() {
			template, keep, err := s.Template("a live log digest", pattern.GetTemplate())
			if err != nil {
				return err
			}
			if !keep {
				continue
			}
			pattern.Template = template
			kept = append(kept, pattern)
		}
		log.Patterns = kept
	}

	// The join keys, through the sanitiser, and this is the load-bearing half of the ordering.
	//
	// FR-102 and contract §9 say the revision, the instance, the trace id and the first-seen
	// instant are **pseudonymised rather than dropped**, because a digest whose keys do not join
	// to the graph is evidence about nothing. Both sides therefore have to produce the SAME
	// token for the same revision — and they only do if the same keyed HMAC computes both.
	//
	// 002's Redactor has its own key and its own construction, so a join key it pseudonymised
	// would be a `px_` token that no graph node carries; and `join_keys.version` it does not
	// pseudonymise at all, which against a graph whose revision names ARE pseudonymised is the
	// same break from the other direction. So this package pseudonymises them first, with the
	// corpus key and the sanitisation table the feeders used, and 002's redactor then passes a
	// `px_`-prefixed value through untouched. One token, both sides.
	for _, keys := range joinKeysIn(resp.GetDigest()) {
		if err := sanitiseJoinKeys(s, keys); err != nil {
			return err
		}
	}
	// The identifiers a digest carries OUTSIDE its join keys. They are the same identifiers under
	// another field name, and a revision pseudonymised in `join_keys.version` while the row beside
	// it spells it out would be both a disclosure and a broken join: a reader matching the two
	// against the graph would find the token and miss the name.
	if versions := resp.GetDigest().GetErrorsByVersion(); versions != nil {
		for _, row := range versions.GetVersions() {
			token, keep, err := s.Field("an errors_by_version row", pathRevision, row.GetVersion())
			if err != nil {
				return err
			}
			if !keep {
				row.Version = ""
				continue
			}
			row.Version = token
		}
	}
	if monitor := resp.GetDigest().GetMonitorState(); monitor != nil {
		if err := sanitiseMonitorState(s, monitor); err != nil {
			return err
		}
	}
	if metric := resp.GetDigest().GetMetric(); metric != nil {
		for _, series := range metric.GetSeries() {
			tags, err := sanitiseTags(s, series.GetTags())
			if err != nil {
				return err
			}
			series.Tags = tags
		}
	}

	// Then 002's redactor, which drops the monitor body and pseudonymises what is left. It leaves
	// our tokens alone, which is the ordering this whole file exists to fix in place.
	return r.Apply(resp)
}

// sanitiseMonitorState pseudonymises the group keys of a monitor-state digest, which name the
// alerting policy the transition belongs to.
//
// The key is a prefixed value — `policy:<name>` — so the prefix is preserved and the identifier
// behind it is tokenised. A reader keeps the ability to tell a policy group from a service group,
// and the policy name itself joins to the ALERT node the feeder recorded under the same token.
func sanitiseMonitorState(s Sanitiser, digest *investigationv1.MonitorStateDigest) error {
	rewrite := func(key string) (string, error) {
		prefix, value, found := strings.Cut(key, ":")
		if !found {
			return key, nil
		}
		path, known := groupKeyPaths[prefix]
		if !known {
			return key, nil
		}
		token, keep, err := s.Field("a monitor-state group key", path, value)
		if err != nil {
			return "", err
		}
		if !keep {
			return prefix + ":", nil
		}
		return prefix + ":" + token, nil
	}
	for _, transition := range digest.GetTransitions() {
		key, err := rewrite(transition.GetGroupKey())
		if err != nil {
			return err
		}
		transition.GroupKey = key
	}
	if len(digest.GetPerGroupState()) > 0 {
		out := make(map[string]string, len(digest.GetPerGroupState()))
		for _, key := range slices.Sorted(maps.Keys(digest.GetPerGroupState())) {
			rewritten, err := rewrite(key)
			if err != nil {
				return err
			}
			out[rewritten] = digest.GetPerGroupState()[key]
		}
		digest.PerGroupState = out
	}
	return nil
}

// groupKeyPaths maps a monitor-state group key's prefix onto the sanitisation table's path.
var groupKeyPaths = map[string]string{
	"policy":  "alertPolicy.name",
	"service": pathService,
	"alert":   "alert_policy_id",
}

// joinKeysIn returns every JoinKeys a digest carries, whatever family it is. It is a switch rather
// than a reflective walk so that a family added to the digest oneof is a compile-time choice about
// whether its keys join, rather than a silent omission.
func joinKeysIn(d *investigationv1.Digest) []*investigationv1.JoinKeys {
	var out []*investigationv1.JoinKeys
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, series := range body.Metric.GetSeries() {
			out = append(out, series.GetJoinKeys())
		}
	case *investigationv1.Digest_Log:
		for _, pattern := range body.Log.GetPatterns() {
			out = append(out, pattern.GetJoinKeys())
		}
	case *investigationv1.Digest_Trace:
		for _, group := range body.Trace.GetGroups() {
			out = append(out, group.GetJoinKeys())
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, version := range body.ErrorsByVersion.GetVersions() {
			out = append(out, version.GetJoinKeys())
		}
	case *investigationv1.Digest_Exemplars:
		for _, exemplar := range body.Exemplars.GetExemplars() {
			out = append(out, exemplar.GetJoinKeys())
		}
	}
	return out
}

// The sanitisation table paths the three populated join keys correspond to. They are the paths the
// FEEDER sanitised the same identifiers under, which is the whole point: one path, one kind, one
// token, on both sides of the join.
const (
	pathRevision = "resource.labels.revision_name"
	pathService  = "resource.labels.service_name"
	pathInstance = "resource.labels.instance_id"
)

// sanitiseJoinKeys pseudonymises the keys in place.
//
// A key the table DROPS is emptied rather than left verbatim, and a dropped key is not a failure:
// FR-102 says these are pseudonymised, so a drop here would mean the table changed underneath this
// code, and carrying the raw value on would be the one outcome worse than losing the join.
func sanitiseJoinKeys(s Sanitiser, keys *investigationv1.JoinKeys) error {
	if keys == nil {
		return nil
	}
	for _, field := range []struct {
		path  string
		value *string
	}{
		{pathRevision, &keys.Version},
		{pathService, &keys.Workload},
		{pathInstance, &keys.PodOrHost},
	} {
		if *field.value == "" {
			continue
		}
		token, keep, err := s.Field("a digest join key", field.path, *field.value)
		if err != nil {
			return err
		}
		if !keep {
			*field.value = ""
			continue
		}
		*field.value = token
	}
	return nil
}

// tagFieldPaths maps a returned series' own label keys onto the sanitisation table's paths. Only
// the infrastructure identifiers are listed: a key that is not here — `response_code_class`, say —
// is a grouping rather than an identity, and is left for 002's redactor, which drops the people
// attributes and leaves the rest.
//
// The map is explicit rather than a `resource.labels.` prefix applied to every key, because the
// table refuses a path it has no rule for: prefixing blindly would turn a new Google label into a
// failed answer rather than an unsanitised one, and neither is what should happen to a grouping.
var tagFieldPaths = map[string]string{
	"project_id":         "resource.labels.project_id",
	"service_name":       pathService,
	"revision_name":      pathRevision,
	"configuration_name": "resource.labels.configuration_name",
	"database_id":        "resource.labels.database_id",
	"instance_id":        pathInstance,
}

// sanitiseTags applies the table to the identifying labels of a series.
func sanitiseTags(s Sanitiser, tags map[string]string) (map[string]string, error) {
	if len(tags) == 0 {
		return tags, nil
	}
	out := make(map[string]string, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		path, known := tagFieldPaths[key]
		if !known {
			out[key] = tags[key]
			continue
		}
		token, keep, err := s.Field("a digest series tag", path, tags[key])
		if err != nil {
			return nil, err
		}
		if keep {
			out[key] = token
		}
	}
	return out, nil
}

// The mandatory coverage block (T092–T094, T098; contract §8, FR-093).
//
// Everything GCP-specific this feature must say about a search it performed is built here, in one
// place, so that a caveat is not written three different ways in three digests. Three of them are
// worth naming.
//
// **Which source the ingestion lag came from.** `MetricDescriptor.metadata.ingestDelay` is a
// documented API field — *"data points older than this age are guaranteed to be ingested and
// available to be read"* — and it is what makes NOT_YET_INGESTED a contractual boundary rather
// than a guess. But it is a *static declared value per metric type*, not a live measure of
// pipeline health, and whether it is populated for `run.googleapis.com/request_count` specifically
// was not verifiable without credentials (research §4). So the fallback is documented, and the
// digest states which of the two it used. A reader who does not know that cannot tell a figure
// Google published from one this code assumed.
//
// **Two instants, and why there is no third.** FR-093 asks for the reference instant the request
// asked about and `coverage.executed_at`, the instant it was executed. It does not ask for an
// observed-time pin, and the reason is not an omission: observed-time pinning has no meaning
// against a live backend, which has no memory of what it used to say. A reproducible investigation
// uses recorded responses, not a re-execution that happens to land on the same numbers.
//
// **`quota_undetermined` is always true on a real-time answer.** GCP reports no in-band remaining
// figure for any of the four APIs this feature reads: there are no rate-limit response headers,
// and the first signal that a quota is gone is HTTP 429. The only vendor-reported usage figure is
// Cloud Monitoring's `serviceruntime` pair, from which remaining must be *computed*, which is
// minutes-stale and itself spends `monitoring.query` quota — a reconciliation signal, not a
// coverage fact (budget.md §3). Reporting it here would put a number in a field a budget manager
// reads as current.

// LagSource says where a reported ingestion lag came from. It is stated in every coverage block,
// because a figure the vendor published and a figure this code assumed are different evidence.
type LagSource string

const (
	// LagDescriptor is `MetricDescriptor.metadata.ingestDelay`, read from the vendor.
	LagDescriptor LagSource = "metric_descriptor_ingest_delay"
	// LagCloudRunDefault is the documented 120 s default for Cloud Run metrics, used when the
	// descriptor carries no figure.
	LagCloudRunDefault LagSource = "documented_default_cloud_run_120s"
	// LagLogReceiveEstimate is `now − max(receiveTimestamp)`, which is an **estimate**: Cloud
	// Logging publishes no equivalent of ingestDelay, and the newest entry a search happened to
	// see is a lower bound on the pipeline's lag rather than a statement about it.
	LagLogReceiveEstimate LagSource = "estimated_from_max_receive_timestamp"
	// LagUndetermined is neither path yielding a figure. It is stated, never omitted.
	LagUndetermined LagSource = "undetermined"
)

// CloudRunIngestDelay is the documented default ingestion delay for `run.googleapis.com/*`, used
// when the metric descriptor's own field is empty (contract §8).
const CloudRunIngestDelay = 120 * time.Second

// coverageInput is what a term implementation states about the search it performed. It is this
// package's own narrowing of engine.CoverageInput: the fields that are always the same here —
// `quota_undetermined`, the execution instant, the lag-source statement — are filled in by
// `coverage` below rather than by each caller, so no term can forget one.
type coverageInput struct {
	// Entities are the graph entities the query was about.
	Entities []string
	// DataSource is the GCP surface and the metric or log scope searched.
	DataSource string
	// Window is the window actually covered, which may be narrower than the one asked for.
	Window *engine.Window
	// Volume is points, entries or groups considered.
	Volume int64
	// VolumeUndetermined says the backend cannot report one.
	VolumeUndetermined bool
	// Sampling is the aggregation actually applied: the alignment period, the per-series
	// aligner and the cross-series reducer, or the log sample's shape.
	Sampling string
	// Criteria are the published truncation and caveat criteria this answer carries, each as
	// "criterion:detail". They are joined with ";" the same way the horizon annotation joins
	// itself on, so a reader parses one field one way.
	Criteria []string
	// Lag and LagSource are the ingestion lag and where it came from.
	Lag       time.Duration
	LagSource LagSource
	// NothingSearch states that nothing was searched at all, which is what an absent source
	// answers. It is separate from a zero volume: "we looked and found none" and "there was
	// nothing to look at" are different sentences, and only the first is evidence.
	NothingSearch bool
}

// coverage builds the mandatory block, filling in what is always true of a GCP answer.
func (b *Backend) coverage(in coverageInput) (*investigationv1.Coverage, error) {
	source := in.DataSource
	if in.LagSource != "" {
		// There is no proto field for "which source the lag came from", and the two fields that
		// might have taken it are both wrong: `sampling` is the aggregation actually applied,
		// and `free_text` is flagged unverified and is never citable on its own (FR-014b) — the
		// last place to put a fact a reader has to act on. `data_source` is free text about
		// what was searched and how, so the statement rides there, in a fixed `key=value`
		// shape a fixture can assert on.
		source += "; ingestion_lag_source=" + string(in.LagSource)
	}
	if in.NothingSearch {
		source += "; searched=nothing"
	}
	return engine.CoverageInput{
		SearchedEntities:   in.Entities,
		DataSource:         source,
		WindowCovered:      in.Window,
		VolumeConsidered:   in.Volume,
		VolumeUndetermined: in.VolumeUndetermined,
		Sampling:           in.Sampling,
		Truncation:         strings.Join(in.Criteria, ";"),
		IngestionLag:       in.Lag,
		// A lag of zero from a source that yielded one is a figure; a lag of zero from no
		// source at all is an absence, and saying "no lag" instead of "lag unknown" is how an
		// empty window inside the pipeline's delay gets read as evidence.
		IngestionLagUndetermined: in.LagSource == LagUndetermined || in.LagSource == "",
		ExecutedAt:               b.now().UTC(),
		// Always. See the file comment: GCP reports no in-band remaining figure, and the
		// serviceruntime figure is a minutes-stale reconciliation signal rather than a
		// coverage fact.
		QuotaUndetermined: true,
	}.Coverage()
}

// criterion formats one published criterion with its detail, for coverageInput.Criteria.
func criterion(name, detail string) string { return name + ":" + detail }

// requestCountCaveats are the two statements every `request_count`-derived digest carries
// (T098, contract §3.1). They are returned together because they are true together: the error
// rate is derived because Cloud Run publishes no error metric, and the metric it is derived from
// omits the failures an incident is most often about.
func requestCountCaveats() []string {
	return []string{
		criterion(CriterionDerivedErrorRate,
			"cloud run publishes no dedicated error metric, so the error rate is derived from "+
				"run.googleapis.com/request_count grouped by metric.labels.response_code_class"),
		criterion(CriterionRequestCountExcludesIngress,
			"request_count excludes requests that never reached the container — unauthorized "+
				"requests rejected at the ingress and requests rejected at max-instances — so "+
				"this digest undercounts exactly those failures"),
	}
}

// metricLag reads one metric type's declared ingestion delay, falling back to the documented
// Cloud Run default and saying which it used.
//
// A descriptor read that FAILS is not a failure of the term: the lag is a statement about the
// pipeline, and a term that refused to answer because it could not fetch one would turn a missing
// caveat into a missing answer. It falls back and says the fallback was used.
func (b *Backend) metricLag(ctx context.Context, metricType string) (time.Duration, LagSource) {
	if b.transport == nil || b.transport.Metrics == nil || metricType == "" {
		return CloudRunIngestDelay, LagCloudRunDefault
	}
	descriptor, err := b.transport.Metrics.GetMetricDescriptor(ctx, b.project, metricType)
	if err != nil {
		return CloudRunIngestDelay, LagCloudRunDefault
	}
	if delay := descriptor.GetMetadata().GetIngestDelay(); delay != nil && delay.AsDuration() > 0 {
		return delay.AsDuration(), LagDescriptor
	}
	return CloudRunIngestDelay, LagCloudRunDefault
}

// logLag estimates the Cloud Logging ingestion lag as `now − max(receiveTimestamp)` over the
// entries a search actually saw, and says it is an estimate.
//
// With no entries there is nothing to estimate from, and the honest answer is that the lag is
// undetermined — not zero. The difference decides whether an empty window is NO_DATA or
// NOT_YET_INGESTED, which is the difference between "nothing happened" and "we cannot tell yet".
func logLag(now, newestReceive time.Time) (time.Duration, LagSource) {
	if newestReceive.IsZero() {
		return 0, LagUndetermined
	}
	lag := now.UTC().Sub(newestReceive.UTC())
	if lag < 0 {
		// The vendor's clock is ahead of ours. A negative lag is not a figure; reporting one
		// would make the ingestion cutoff run backwards.
		return 0, LagUndetermined
	}
	return lag, LagLogReceiveEstimate
}

// ingested reports whether a window's end is outside the ingestion lag — the cutoff of contract
// §8, written once so that no term computes it slightly differently:
//
//	cutoff := now − ingestDelay
//	window.End >  cutoff, no points → NOT_YET_INGESTED
//	window.End <= cutoff, no points → NO_DATA
//
// A lag from no source at all is not a lag of zero: with the figure undetermined the cutoff is
// unknown, and the honest reading of an empty window is NOT_YET_INGESTED, which claims nothing.
func ingested(now time.Time, window *engine.Window, lag time.Duration, source LagSource) bool {
	if window.GetEnd() == nil {
		return false
	}
	if source == LagUndetermined || source == "" {
		return false
	}
	return !window.GetEnd().AsTime().UTC().After(now.UTC().Add(-lag))
}
