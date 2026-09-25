// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	algebra "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// The manifest `investigate to-incident` writes (FR-055, SC-012, contracts/incident-format.md).
//
// FR-055 says a reviewed investigation is exportable as a replayable incident "accepted by the
// evaluation harness with no hand-editing". The export already wrote the replayable half — the
// events, both recording layers and the decision record — but wrote no `manifest.yaml`, so what
// came out was a directory `fixture verify` did not recognise as a fixture at all. This file
// writes the other half.
//
// Two rules decide what goes in it, and they pull against each other:
//
//   - **Everything derivable is derived.** The question is the intake, verbatim. The ground
//     truth's culprit is the reviewer's validated root cause — never the engine's own top
//     hypothesis, because a corpus that learned from the engine's answer would be measuring the
//     engine against itself. The prior rank is read off the ledger's own priors, the knowability
//     time off the observed instants of the evidence that settled it, the sources off the
//     exported events, the clock off the run's window.
//   - **Everything that is not derivable is written as a placeholder and SAID to be one.** A
//     causal path's mechanism hops, the predicates that make evidence decisive, the decoys and
//     what exonerates each of them: these are a reviewer's judgements and no amount of reading
//     the ledger produces them. They are written structurally valid — so `LoadManifest` accepts
//     the file and the reviewer can run `fixture verify` on the way in rather than at the end —
//     and marked `# TODO(reviewer)`, which `fixture verify` reads back and reports. A fixture
//     with placeholders in it is a fixture in progress, not a silently wrong ground truth.
//
// The placeholder marker is a comment rather than a sentinel value because a sentinel would have
// to be legal everywhere it could appear — in a ref, in an instant, in a float — and one that is
// legal everywhere is one that can be scored. A comment cannot be scored by construction.

// incidentManifestYAML renders the manifest for a reviewed investigation.
//
// `dir` is the directory the incident is being written into: its base name is the fixture id,
// because 001's loader requires the two to agree.
func incidentManifestYAML(dir string, inv *investigationv1.Investigation) (string, error) {
	review := inv.GetReviews()[0]
	symptom := openingSymptom(inv)
	if symptom == nil {
		return "", fmt.Errorf("investigate to-incident %s: the investigation carries no symptom, so "+
			"there is no question to write into the manifest", inv.GetInvestigationId())
	}
	culprit := strings.TrimSpace(review.GetValidatedRootCause())
	subject := subjectRefOfSymptom(symptom)

	var b strings.Builder
	fmt.Fprintf(&b, "# Recorded from investigation %s by `aisre investigate to-incident` "+
		"(FR-055).\n", inv.GetInvestigationId())
	b.WriteString("#\n")
	b.WriteString("# Every `# TODO(reviewer)` below marks a field this command could not derive: a\n" +
		"# reviewer's judgement, not a fact in the ledger. `fixture verify` lists them; the\n" +
		"# fixture is not corpus-ready until none are left.\n")
	fmt.Fprintf(&b, "id: %s\n", yamlScalar(filepath.Base(dir)))
	b.WriteString("family: recorded-incident   # TODO(reviewer): the behaviour family this joins\n")
	fmt.Fprintf(&b, "description: %s\n", yamlScalar(incidentDescription(inv, review)))
	fmt.Fprintf(&b, "schema_version: %s\n", yamlScalar(defaulted(inv.GetSchemaVersion(), "1.0.0")))
	b.WriteString("hand_authored: true\n")
	b.WriteString("events: events.jsonl\n\n")

	b.WriteString("sources:\n")
	sources, err := exportedSources(dir)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("investigate to-incident %s: the export wrote no events, so no source "+
			"can be declared; 001's loader requires at least one", inv.GetInvestigationId())
	}
	for _, src := range sources {
		fmt.Fprintf(&b, "  - source_id: %s\n    kind: %s\n    ordering: none\n",
			yamlScalar(src), yamlScalar(sourceKind(src)))
	}
	b.WriteString("  # TODO(reviewer): `ordering` and `reordering_window` are the conservative\n" +
		"  # default; the shuffle check permutes within the window each feeder actually declares.\n\n")

	start, end := clockOf(inv)
	fmt.Fprintf(&b, "clock:\n  start: %s\n  end:   %s\n\n", instant(start), instant(end))
	b.WriteString("# No golden queries: the export carries the event log, and a reviewer adds the\n" +
		"# queries this incident should pin.\nqueries: []\n\n")

	b.WriteString("incident:\n")
	writeIncidentQuestion(&b, inv, symptom, subject)
	writeIncidentGroundTruth(&b, inv, culprit, subject)
	writeIncidentWorld(&b, inv)
	b.WriteString("\n  runs: 3\n")
	return b.String(), nil
}

func writeIncidentQuestion(b *strings.Builder, inv *investigationv1.Investigation,
	symptom *investigationv1.Symptom, subject string) {
	b.WriteString("  question:\n")
	fmt.Fprintf(b, "    transport: %s\n", yamlScalar(defaulted(symptom.GetTransport(), "monitor")))
	fmt.Fprintf(b, "    origin_system: %s\n", yamlScalar(symptom.GetOriginSystem()))
	fmt.Fprintf(b, "    origin_ref: %s\n", yamlScalar(symptom.GetOriginRef()))
	fmt.Fprintf(b, "    statement: %s\n", yamlScalar(symptom.GetStatement()))
	fmt.Fprintf(b, "    fired_at: %s\n", instant(symptom.GetFiredAt().AsTime()))
	if subject != "" {
		fmt.Fprintf(b, "    subject: %s\n", yamlScalar(subject))
	} else {
		b.WriteString("    unparseable_subject: \"\"   # TODO(reviewer): the question resolved to no " +
			"ref; name what arrived\n")
	}
	if lookback := lookbackOf(inv); lookback != "" {
		fmt.Fprintf(b, "    lookback: %s\n", yamlScalar(lookback))
	}
	if profile := inv.GetSpend().GetLimits().GetName(); profile != "" {
		fmt.Fprintf(b, "    profile: %s\n", yamlScalar(profile))
	}
	if s := symptom.GetSeverity(); s != "" {
		fmt.Fprintf(b, "    severity: %s\n", yamlScalar(s))
	}
	if t := symptom.GetTitle(); t != "" {
		fmt.Fprintf(b, "    title: %s\n", yamlScalar(t))
	}
	if d := symptom.GetDeclaringIdentity(); d != "" {
		fmt.Fprintf(b, "    declaring_identity: %s\n", yamlScalar(d))
	}
}

func writeIncidentGroundTruth(b *strings.Builder, inv *investigationv1.Investigation,
	culprit, subject string) {
	b.WriteString("\n  ground_truth:\n")
	fmt.Fprintf(b, "    culprit: %s   # the reviewer's validated root cause, never the engine's\n",
		yamlScalar(culprit))

	// The causal path. Where the culprit is a change, the path must start at it and end at the
	// symptom (the loader checks both); the hops in between are the mechanism and are exactly
	// what nothing here can derive.
	b.WriteString("    causal_path:\n")
	if isChangeCulprit(culprit) && subject != "" && culprit != subject {
		fmt.Fprintf(b, "      - entity: %s\n        via: changed-by\n        role: cause\n",
			yamlScalar(culprit))
		fmt.Fprintf(b, "      # TODO(reviewer): the mechanism hops between the change and the "+
			"symptom.\n      - entity: %s\n        role: symptom\n", yamlScalar(subject))
	} else {
		target := subject
		if target == "" {
			target = culprit
		}
		fmt.Fprintf(b, "      # TODO(reviewer): the path that localises the symptom.\n"+
			"      - entity: %s\n        role: symptom\n", yamlScalar(target))
	}

	b.WriteString("\n    decisive_evidence:\n")
	b.WriteString("      # TODO(reviewer): predicates over the recorded digests, never strings. The\n" +
		"      # placeholder below is structurally valid so the fixture loads; it settles nothing.\n" +
		"      - term:\n          monitor_state: {}\n        field_path: state\n        op: present\n")

	b.WriteString("\n    decoys: []\n")
	b.WriteString("    # TODO(reviewer): every non-culprit candidate the prior ranks, with its causal\n" +
		"    # role, and what exonerates it under `exonerating_evidence`.\n")
	b.WriteString("    exonerating_evidence: {}\n")

	fmt.Fprintf(b, "\n    knowability_time: %s\n", instant(knowabilityTime(inv)))
	b.WriteString("    # derived: the latest observed instant among the evidence behind the top\n" +
		"    # hypothesis. Before it, `unknown` is the correct answer.\n")
	if rank := priorRankOfCulprit(inv, culprit); rank > 0 {
		fmt.Fprintf(b, "    prior_rank_of_culprit: %d\n", rank)
	} else {
		b.WriteString("    prior_rank_of_culprit: 0   # the prior does not rank the culprit at all\n")
	}
	b.WriteString("\n    provenance:\n      kind: recorded\n")
	// Derived, not a placeholder: the recording beside this manifest was redacted by the worker
	// boundary under exactly this policy, and the fixture is the recording.
	fmt.Fprintf(b, "      sanitiser_policy_version: %s\n",
		yamlScalar(algebra.DefaultRedactionPolicyVersion))
}

func writeIncidentWorld(b *strings.Builder, inv *investigationv1.Investigation) {
	reference := knowabilityTime(inv)
	b.WriteString("\n  world:\n")
	// The defaults of contracts/incident-format.md. `fixture record-world` reads them back
	// rather than taking flags, so a reviewer who widens one has to widen it here.
	fmt.Fprintf(b, "    hop_radius: 2\n    drill_down_depth: 1\n")
	fmt.Fprintf(b, "    window_grid:\n      - {reference_at: %s, width_seconds: 900}\n"+
		"      - {reference_at: %s, width_seconds: 3600}\n", instant(reference), instant(reference))
	b.WriteString("    # TODO(reviewer): the grid is centred on the knowability time; centre it on\n" +
		"    # the labelled onset once `onset:` is filled in.\n")
	fmt.Fprintf(b, "    algebra_version: %s\n",
		yamlScalar(defaulted(inv.GetAlgebraVersion(), "1.0.0")))
	b.WriteString("    miss_rate_threshold: 0.05\n    shape: step\n")
}

// --- derivations -----------------------------------------------------------------------------

// openingSymptom is the symptom that opened the incident: the earliest one that did not attach to
// an investigation already running.
func openingSymptom(inv *investigationv1.Investigation) *investigationv1.Symptom {
	var out *investigationv1.Symptom
	for _, s := range inv.GetSymptoms() {
		if s.GetAttachedAsAdditional() {
			continue
		}
		if out == nil || s.GetFiredAt().AsTime().Before(out.GetFiredAt().AsTime()) {
			out = s
		}
	}
	if out == nil && len(inv.GetSymptoms()) > 0 {
		out = inv.GetSymptoms()[0]
	}
	return out
}

// subjectRefOfSymptom is the `<namespace>=<value>` the question named, which is what the manifest
// spells a subject as.
func subjectRefOfSymptom(symptom *investigationv1.Symptom) string {
	for _, named := range symptom.GetNamedIdentifiers() {
		ref := graph.RefFromProto(named)
		if !ref.IsZero() {
			return ref.String()
		}
	}
	return ""
}

// knowabilityTime is the latest observed instant among the evidence items the top-ranked
// hypothesis rests on: the instant at which the answer became knowable (FR-061b). A run with no
// such evidence falls back to the investigation's own observed instant, which is the earliest
// instant the manifest could honestly claim.
func knowabilityTime(inv *investigationv1.Investigation) time.Time {
	observed := map[string]time.Time{}
	for _, e := range inv.GetLedger().GetEvidence() {
		observed[e.GetEvidenceId()] = e.GetObservedAt().AsTime().UTC()
	}
	var best time.Time
	for _, h := range inv.GetLedger().GetHypotheses() {
		if h.GetRank() != 1 {
			continue
		}
		for _, id := range h.GetSupportingEvidenceIds() {
			if at, ok := observed[id]; ok && at.After(best) {
				best = at
			}
		}
	}
	if best.IsZero() {
		best = inv.GetObservedAt().AsTime().UTC()
	}
	return best
}

// priorRankOfCulprit is the deterministic ranker's own 1-based rank for the culprit, read off the
// ledger's priors rather than off the final ranks — the final rank is what the evidence did, and
// lift is the difference between the two (FR-062b). Zero means the prior does not rank it.
func priorRankOfCulprit(inv *investigationv1.Investigation, culprit string) int {
	ranked := make([]*investigationv1.Hypothesis, 0, len(inv.GetLedger().GetHypotheses()))
	for _, h := range inv.GetLedger().GetHypotheses() {
		if h.GetCandidateChangeEntityId() != "" {
			ranked = append(ranked, h)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].GetPrior() != ranked[j].GetPrior() {
			return ranked[i].GetPrior() > ranked[j].GetPrior()
		}
		return ranked[i].GetRank() < ranked[j].GetRank()
	})
	for i, h := range ranked {
		if h.GetCandidateChangeEntityId() == culprit {
			return i + 1
		}
	}
	return 0
}

// clockOf bounds the fixture's window with the run's own.
func clockOf(inv *investigationv1.Investigation) (time.Time, time.Time) {
	start := inv.GetWindow().GetStart().AsTime().UTC()
	end := inv.GetWindow().GetEnd().AsTime().UTC()
	if start.IsZero() {
		start = inv.GetValidAt().AsTime().UTC()
	}
	if end.IsZero() || !end.After(start) {
		end = inv.GetObservedAt().AsTime().UTC()
	}
	return start, end
}

func lookbackOf(inv *investigationv1.Investigation) string {
	start := inv.GetWindow().GetStart().AsTime()
	end := inv.GetWindow().GetEnd().AsTime()
	if start.IsZero() || !end.After(start) {
		return ""
	}
	return end.Sub(start).String()
}

// exportedSources reads the distinct source ids out of the events the export wrote. They are the
// feeders 001's loader registers before the first event, and deriving them from the events is the
// only spelling that cannot disagree with them.
func exportedSources(dir string) ([]string, error) {
	path := filepath.Join(dir, "events.jsonl")
	file, err := os.Open(path) //nolint:gosec // the operator's own --out directory
	if err != nil {
		return nil, fmt.Errorf("investigate to-incident: read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var event struct {
			SourceID string `json:"sourceId"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("investigate to-incident: %s is not canonical JSON: %w", path, err)
		}
		if event.SourceID != "" {
			seen[event.SourceID] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("investigate to-incident: read %s: %w", path, err)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// sourceKind is the connector family a source id names, which by convention is everything before
// the first colon (`k8s:demo` → `k8s`). A source id with no colon is its own kind.
func sourceKind(sourceID string) string {
	if family, _, ok := strings.Cut(sourceID, ":"); ok && family != "" {
		return family
	}
	return sourceID
}

func isChangeCulprit(culprit string) bool {
	switch {
	case culprit == fixture.CulpritUnobserved,
		strings.HasPrefix(culprit, fixture.CulpritNotChangeInduced):
		return false
	}
	_, err := graph.ParseRef(culprit)
	return err == nil
}

func incidentDescription(inv *investigationv1.Investigation, review *investigationv1.HumanReview) string {
	verdict := inv.GetVerdictLine()
	if verdict == "" {
		verdict = "the run recorded no verdict line"
	}
	return fmt.Sprintf("Recorded from investigation %s. The engine said: %s The reviewer validated "+
		"%q (%s). The `world/` and `trajectories/` layers beside this manifest are the run's own "+
		"recording; the goldens and the decoys are a reviewer's to add.",
		inv.GetInvestigationId(), verdict, review.GetValidatedRootCause(), review.GetAuthor())
}

func defaulted(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func instant(at time.Time) string {
	if at.IsZero() {
		return "1970-01-01T00:00:00Z"
	}
	return at.UTC().Format(time.RFC3339)
}

// yamlScalar renders one string as a YAML scalar, through the encoder that will read it back, so
// a verdict line carrying a colon or a quote cannot break the file it is written into.
func yamlScalar(s string) string {
	raw, err := yaml.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.TrimRight(string(raw), "\n")
}
