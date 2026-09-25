// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
)

// The `incident:` block of a fixture manifest (002 contracts/incident-format.md, FR-061b,
// FR-063).
//
// An incident fixture is a feature 001 fixture directory with three additions — this block, a
// `world/` directory and a `trajectories/` directory — and **no new input format**. That is why
// the block is parsed here, beside the 001 manifest, rather than in a harness of its own: the
// manifest loader decodes strictly (`KnownFields`), so a fixture carrying an `incident:` key
// this package did not know would be refused as broken. Every key an incident fixture may write
// is therefore named in this file, and a key that is not is a fixture bug rather than a silently
// ignored instruction.
//
// What this file does **not** do is score anything. Grading a run against `ground_truth` is the
// evaluation report's job (T109) and trajectory replay is T096's; both arrive later and both
// read the shapes defined here. What is needed *now* is that the ground truth a fixture publishes
// is well formed at the moment the fixture is written — a corpus whose ground truth is only
// validated when the scorer runs is a corpus that is wrong for as long as the scorer is missing.

// CulpritUnobserved is the ground truth of an incident no observed change explains: the cause
// was real and the graph could not see it (FR-071b, SC-023).
const CulpritUnobserved = "unobserved"

// CulpritNotChangeInduced is the ground truth of an incident that no change caused at all. It
// always carries a category, either as `not_change_induced: <category>` or in the sibling
// `category` field.
const CulpritNotChangeInduced = "not_change_induced"

// Incident is the `incident:` block: what the on-call was asked, what the truth is, how the
// world beside the fixture was recorded, and how many runs a scored verification takes.
type Incident struct {
	// Question is the intake: the alert or declaration the investigation starts from.
	Question IncidentQuestion `yaml:"question"`
	// GroundTruth is the published answer shape of FR-061b.
	GroundTruth IncidentGroundTruth `yaml:"ground_truth"`
	// World declares the shape the `world/` layer was recorded at. `fixture record-world`
	// reads it back rather than taking flags, so a re-record cannot change a fixture silently.
	World IncidentWorld `yaml:"world"`
	// Runs is how many times a scored verification investigates the question.
	Runs int `yaml:"runs"`
}

// IncidentQuestion is the intake half of the block.
type IncidentQuestion struct {
	// Transport is how the question arrived: monitor, human_declared or explicit_reference.
	Transport string `yaml:"transport"`
	// OriginSystem and OriginRef identify the question in the system that raised it.
	OriginSystem string `yaml:"origin_system"`
	OriginRef    string `yaml:"origin_ref"`
	// Statement is what the on-call was told.
	Statement string `yaml:"statement"`
	// FiredAt is the instant of the transition or the declaration.
	FiredAt time.Time `yaml:"fired_at"`
	// Subject is the entity the question names, `<namespace>=<value>`. For a fixture whose
	// point is that the subject cannot be parsed, it is empty and `unparseable_subject` says
	// what arrived instead.
	Subject string `yaml:"subject"`
	// UnparseableSubject is the raw target text of a declaration no slug rule could resolve.
	// It is deliberately a separate key from Subject: "no subject" and "a subject nobody could
	// read" are different questions and only the second one has a resolving action.
	UnparseableSubject string `yaml:"unparseable_subject"`
	// Lookback is how far back the question reaches, as a Go duration.
	Lookback string `yaml:"lookback"`
	// Profile is the budget profile the question is asked under.
	Profile string `yaml:"profile"`
	// Severity, Title and DeclaringIdentity are carried by a human declaration (FR-002a).
	Severity          string `yaml:"severity"`
	Title             string `yaml:"title"`
	DeclaringIdentity string `yaml:"declaring_identity"`
	// AttachesLater names the further alert transitions that must join this investigation
	// rather than starting one of their own (FR-008b, FR-008c). Each is `<origin_ref>@<instant>`
	// as the event spells it, so the assertion reads against events.jsonl.
	AttachesLater []string `yaml:"attaches_later"`
}

// IncidentGroundTruth is the published ground-truth shape (FR-061b).
type IncidentGroundTruth struct {
	// Culprit is a change identifier, `unobserved`, or `not_change_induced: <category>`.
	Culprit string `yaml:"culprit"`
	// Category is the cause category, for the two culprit spellings that carry one. It may be
	// written here or after the colon in Culprit; Class and CauseCategory read both.
	Category string `yaml:"category"`
	// CausalPath is the chain from cause to symptom, so localisation, attribution and
	// mechanism are graded separately and with partial credit. The last step is the symptom
	// and names no `via`.
	CausalPath []CausalStep `yaml:"causal_path"`
	// DecisiveEvidence is what settles the question, as predicates over response digests
	// rather than as strings.
	DecisiveEvidence []EvidencePredicate `yaml:"decisive_evidence"`
	// Decoys are the candidates that are not the culprit, each with the causal role that
	// explains why it looks plausible. Every decoy is a key of ExoneratingEvidence.
	Decoys []Decoy `yaml:"decoys"`
	// ExoneratingEvidence is what rules each decoy out, keyed by the decoy's identifier. An
	// empty list means the decoy cannot be ruled out by the evidence in this fixture, which is
	// only legal for a decoy whose role is `not_separable` (FR-025).
	ExoneratingEvidence map[string][]EvidencePredicate `yaml:"exonerating_evidence"`
	// KnowabilityTime is the earliest observed instant at which the decisive evidence existed.
	// Asked before it, `unknown` is the correct answer and naming the culprit is a failure.
	KnowabilityTime time.Time `yaml:"knowability_time"`
	// Onset is the labelled symptom onset and the tolerance an estimate is graded within.
	Onset *IncidentOnset `yaml:"onset"`
	// PriorRankOfCulprit is the deterministic ranker's own rank for the culprit, so lift is
	// measurable where the prior fails (FR-062b). Zero means the prior does not rank it at
	// all; it is a pointer so that "unranked" and "not stated" stay distinguishable.
	PriorRankOfCulprit *int `yaml:"prior_rank_of_culprit"`
	// Provenance is where the fixture came from and under which sanitiser policy.
	Provenance IncidentProvenance `yaml:"provenance"`
}

// CausalStep is one hop of the causal path.
type CausalStep struct {
	// Entity is the node, `<namespace>=<value>`.
	Entity string `yaml:"entity"`
	// Via is the edge to the next step, e.g. `changed-by`, `calls`. The last step names none.
	Via string `yaml:"via"`
	// Role is what this step is in the story: `cause`, `mechanism` or `symptom`. It is
	// optional; where it is absent the position in the path says.
	Role string `yaml:"role"`
}

// Decoy is a candidate that is not the culprit, and why it looked like one.
type Decoy struct {
	// Entity is the candidate's identifier.
	Entity string `yaml:"entity"`
	// CausalRole is one of decoyRoles: what the candidate actually is.
	CausalRole string `yaml:"causal_role"`
	// Why is the human sentence a reviewer reads.
	Why string `yaml:"why"`
}

// decoyRoles is the published set of causal roles a decoy may carry.
//
//   - candidate_effect — it happened *after* the onset, so it is downstream of the symptom
//     rather than upstream of it (an autoscaler reacting to the load the culprit caused);
//   - coincident — it happened near the onset and touches nothing on the causal path;
//   - stale — it is old enough that the symptom would have appeared earlier;
//   - out_of_scope — it is outside the neighbourhood and must be excluded rather than ranked;
//   - unattached — its target is a thing no source has described;
//   - not_separable — the evidence in this fixture cannot tell it from the culprit, and both
//     must be reported together (FR-025). It is the one role whose exonerating evidence is
//     legitimately empty.
var decoyRoles = []string{
	"candidate_effect", "coincident", "stale", "out_of_scope", "unattached", "not_separable",
}

// EvidencePredicate is one assertion over a response digest: which term answers it, which field
// of that answer, and what has to be true of it. It is a predicate rather than a string so that
// grading cannot be satisfied by prose that happens to contain the right words.
type EvidencePredicate struct {
	// Term is the algebra term whose answer carries the field, as a single-key mapping naming
	// the family, e.g. `{errors_by_version: {...}}`.
	Term map[string]any `yaml:"term"`
	// FieldPath addresses the field inside the digest, e.g.
	// `versions[version=rev7].error_rate`.
	FieldPath string `yaml:"field_path"`
	// Op is one of predicateOps.
	Op string `yaml:"op"`
	// Value is what the field is compared against. It is untyped because a predicate compares
	// numbers, instants, strings and booleans.
	Value any `yaml:"value"`
	// Note is the sentence a reviewer reads to see why this predicate is decisive.
	Note string `yaml:"note"`
}

// predicateOps is the published comparison set.
var predicateOps = []string{
	"eq", "ne", "lt", "lte", "gt", "gte", "contains", "not_contains", "present", "absent",
}

// termFamilies is the algebra's family set (api/sreagent/investigation/v1, AlgebraTerm). A
// predicate naming a family that does not exist is a predicate nothing can ever evaluate.
var termFamilies = []string{
	"graph", "compare", "onset", "new_log_patterns", "error_spans", "errors_by_version",
	"monitor_state", "exemplars", "drill_down", "knowledge_search",
}

// IncidentOnset is the labelled onset and the tolerance an estimate is graded within.
type IncidentOnset struct {
	At               time.Time `yaml:"at"`
	ToleranceSeconds int       `yaml:"tolerance_seconds"`
}

// IncidentProvenance is where a fixture came from (FR-061b).
type IncidentProvenance struct {
	// Kind is `synthetic`, `recorded` or `derived`.
	Kind string `yaml:"kind"`
	// SanitiserPolicyVersion is the policy version that produced it.
	SanitiserPolicyVersion string `yaml:"sanitiser_policy_version"`
	// DerivedFrom names the parent of a derived fixture.
	DerivedFrom string `yaml:"derived_from"`
	// Transformation names what was applied to the parent.
	Transformation string `yaml:"transformation"`
}

// provenanceKinds is the published set.
var provenanceKinds = []string{"synthetic", "recorded", "derived"}

// IncidentWorld declares the shape of the `world/` layer.
type IncidentWorld struct {
	// HopRadius is the neighbourhood the cross product was taken over.
	HopRadius int `yaml:"hop_radius"`
	// DrillDownDepth is how far handles were followed. Deeper answers `not_recorded`, by design.
	DrillDownDepth int `yaml:"drill_down_depth"`
	// WindowGrid is the before/after pairs the cross product was taken over.
	WindowGrid []IncidentWindow `yaml:"window_grid"`
	// AlgebraVersion is the term algebra the keys were computed under.
	AlgebraVersion string `yaml:"algebra_version"`
	// MissRateThreshold gates the fixture: above it the fixture is reported as insufficient
	// and is not scored.
	MissRateThreshold float64 `yaml:"miss_rate_threshold"`
	// Shape names the degradation shape the generator stamped onto the scenario this world was
	// recorded from — `step`, `slow-burn`, `distant-culprit`, `adjacent-decoy`
	// (pkg/backend/synthetic, contracts/incident-format.md §world).
	//
	// It is a property of the *story* rather than of the graph: nothing about "this change made
	// its targets worse" says whether it did so at once, over an hour, or only once demand
	// climbed into a ceiling it lowered, and a generator cannot read that out of an event log.
	// Empty means `step`, which is what every world recorded before the shapes existed holds, so
	// the key is additive and a manifest that says nothing keeps the fixture it already had. A
	// name the generator does not publish is refused here rather than silently defaulted: a
	// fixture that asked for a shape and got another one would be a different fixture under the
	// same id.
	Shape string `yaml:"shape"`
	// Note is where a fixture states the caps it chose and why — the grid it recorded over, the
	// statistic set the recorder covers, the size the result came to. A world is the largest
	// thing a fixture ships and the cap is a judgement; a judgement nobody wrote down is one
	// the next author will quietly undo.
	Note string `yaml:"note"`
}

// IncidentWindow is one entry of the window grid.
type IncidentWindow struct {
	ReferenceAt  time.Time `yaml:"reference_at"`
	WidthSeconds int       `yaml:"width_seconds"`
}

// WorldDir is where a fixture's recorded world lives.
func (m *Manifest) WorldDir() string { return filepath.Join(m.Dir, "world") }

// TrajectoriesDir is where a fixture's recorded runs live. It is absent until a run has been
// recorded, which is Phase 7's work (T093): an incident fixture with no trajectories is a normal
// state of the corpus, not a broken fixture.
func (m *Manifest) TrajectoriesDir() string { return filepath.Join(m.Dir, "trajectories") }

// Class returns the ground truth's cause class: `change_induced` for a culprit that names a
// change, or `unobserved` / `not_change_induced`.
func (g IncidentGroundTruth) Class() string {
	trimmed := strings.TrimSpace(g.Culprit)
	switch {
	case trimmed == CulpritUnobserved:
		return CulpritUnobserved
	case trimmed == CulpritNotChangeInduced,
		strings.HasPrefix(trimmed, CulpritNotChangeInduced+":"):
		return CulpritNotChangeInduced
	default:
		return "change_induced"
	}
}

// CauseCategory returns the category, read from either spelling.
func (g IncidentGroundTruth) CauseCategory() string {
	trimmed := strings.TrimSpace(g.Culprit)
	if rest, ok := strings.CutPrefix(trimmed, CulpritNotChangeInduced+":"); ok {
		if category := strings.TrimSpace(rest); category != "" {
			return category
		}
	}
	return strings.TrimSpace(g.Category)
}

// Validate refuses an incident block that could not be graded.
//
// It is called from LoadManifest, so a malformed ground truth is a load error at the moment
// somebody writes the fixture rather than a scoring surprise months later.
func (i *Incident) Validate(path string) error {
	if i == nil {
		return nil
	}
	if err := i.Question.validate(path); err != nil {
		return err
	}
	if err := i.GroundTruth.validate(path); err != nil {
		return err
	}
	if err := i.World.validate(path); err != nil {
		return err
	}
	if i.Runs <= 0 {
		return fmt.Errorf("fixture: %s: incident.runs must be positive; a scored fixture is investigated at least once", path)
	}
	return nil
}

func (q IncidentQuestion) validate(path string) error {
	switch q.Transport {
	case "monitor", "human_declared", "explicit_reference":
	default:
		return fmt.Errorf("fixture: %s: incident.question.transport %q is not published; want monitor, human_declared or explicit_reference",
			path, q.Transport)
	}
	if strings.TrimSpace(q.Statement) == "" {
		return fmt.Errorf("fixture: %s: incident.question.statement is required; it is what the on-call was told", path)
	}
	if q.FiredAt.IsZero() {
		return fmt.Errorf("fixture: %s: incident.question.fired_at is required", path)
	}
	switch {
	case q.Subject != "":
		if _, err := graph.ParseRef(q.Subject); err != nil {
			return fmt.Errorf("fixture: %s: incident.question.subject %q: %w", path, q.Subject, err)
		}
	case q.UnparseableSubject == "":
		return fmt.Errorf("fixture: %s: incident.question needs a subject, or an unparseable_subject "+
			"for a question whose point is that no rule could read one", path)
	}
	if q.Lookback != "" {
		if _, err := time.ParseDuration(q.Lookback); err != nil {
			return fmt.Errorf("fixture: %s: incident.question.lookback %q: %w", path, q.Lookback, err)
		}
	}
	if q.Transport == "human_declared" && strings.TrimSpace(q.DeclaringIdentity) == "" {
		return fmt.Errorf("fixture: %s: a human_declared question names no declaring_identity; "+
			"an unsigned declaration is not an intake (FR-002a)", path)
	}
	return nil
}

func (g IncidentGroundTruth) validate(path string) error {
	if strings.TrimSpace(g.Culprit) == "" {
		return fmt.Errorf("fixture: %s: incident.ground_truth.culprit is required: a change identifier, "+
			"%q, or %q with a category (FR-061b)", path, CulpritUnobserved, CulpritNotChangeInduced)
	}
	switch g.Class() {
	case CulpritNotChangeInduced:
		if g.CauseCategory() == "" {
			return fmt.Errorf("fixture: %s: a %s ground truth must carry a category, either as "+
				"`%s: <category>` or in the sibling `category` key (FR-071b)",
				path, CulpritNotChangeInduced, CulpritNotChangeInduced)
		}
	case "change_induced":
		if _, err := graph.ParseRef(g.Culprit); err != nil {
			return fmt.Errorf("fixture: %s: incident.ground_truth.culprit %q is neither %q, %q nor a "+
				"`<namespace>=<value>` change identifier: %w",
				path, g.Culprit, CulpritUnobserved, CulpritNotChangeInduced, err)
		}
	}

	if len(g.CausalPath) == 0 {
		return fmt.Errorf("fixture: %s: incident.ground_truth.causal_path is empty; even where no change "+
			"explains the symptom the path localises it, which is what SC-023 grades", path)
	}
	for idx, step := range g.CausalPath {
		if _, err := graph.ParseRef(step.Entity); err != nil {
			return fmt.Errorf("fixture: %s: incident.ground_truth.causal_path[%d].entity %q: %w",
				path, idx, step.Entity, err)
		}
		last := idx == len(g.CausalPath)-1
		if last && step.Via != "" {
			return fmt.Errorf("fixture: %s: incident.ground_truth.causal_path[%d] is the symptom and "+
				"names `via: %s`; the last step is where the path ends", path, idx, step.Via)
		}
		if !last && step.Via == "" {
			return fmt.Errorf("fixture: %s: incident.ground_truth.causal_path[%d] names no `via`; a hop "+
				"with no edge is an assertion the graph cannot check", path, idx)
		}
	}
	if g.Class() == "change_induced" && g.CausalPath[0].Entity != g.Culprit {
		return fmt.Errorf("fixture: %s: the causal path starts at %q but the culprit is %q; the path "+
			"runs from cause to symptom", path, g.CausalPath[0].Entity, g.Culprit)
	}

	if len(g.DecisiveEvidence) == 0 {
		return fmt.Errorf("fixture: %s: incident.ground_truth.decisive_evidence is empty; a fixture that "+
			"names no decisive evidence cannot say when its answer became knowable (FR-061b)", path)
	}
	for idx, predicate := range g.DecisiveEvidence {
		if err := predicate.validate(fmt.Sprintf("decisive_evidence[%d]", idx), path); err != nil {
			return err
		}
	}

	declared := make(map[string]string, len(g.Decoys))
	for idx, decoy := range g.Decoys {
		if strings.TrimSpace(decoy.Entity) == "" {
			return fmt.Errorf("fixture: %s: incident.ground_truth.decoys[%d] names no entity", path, idx)
		}
		if !slices.Contains(decoyRoles, decoy.CausalRole) {
			return fmt.Errorf("fixture: %s: incident.ground_truth.decoys[%d].causal_role %q is not "+
				"published; want one of %v", path, idx, decoy.CausalRole, decoyRoles)
		}
		if _, dup := declared[decoy.Entity]; dup {
			return fmt.Errorf("fixture: %s: incident.ground_truth.decoys names %s twice", path, decoy.Entity)
		}
		declared[decoy.Entity] = decoy.CausalRole
	}
	for _, entity := range sortedEvidenceKeys(g.ExoneratingEvidence) {
		if _, known := declared[entity]; !known {
			return fmt.Errorf("fixture: %s: incident.ground_truth.exonerating_evidence names %s, which is "+
				"not in `decoys`; every candidate that is ruled out has to say what it was", path, entity)
		}
		for idx, predicate := range g.ExoneratingEvidence[entity] {
			if err := predicate.validate(fmt.Sprintf("exonerating_evidence[%s][%d]", entity, idx), path); err != nil {
				return err
			}
		}
	}
	for entity, role := range declared {
		predicates, present := g.ExoneratingEvidence[entity]
		if !present {
			return fmt.Errorf("fixture: %s: decoy %s has no entry under exonerating_evidence; a decoy "+
				"with nothing against it is a second culprit (FR-061b)", path, entity)
		}
		if len(predicates) == 0 && role != "not_separable" {
			return fmt.Errorf("fixture: %s: decoy %s is exonerated by nothing, but its causal_role is %q; "+
				"only `not_separable` may be ruled out by no evidence (FR-025)", path, entity, role)
		}
	}

	if g.KnowabilityTime.IsZero() {
		return fmt.Errorf("fixture: %s: incident.ground_truth.knowability_time is required; before it, "+
			"`unknown` is the correct answer and the fixture has to say so (FR-061b)", path)
	}
	if g.Onset != nil {
		if g.Onset.At.IsZero() {
			return fmt.Errorf("fixture: %s: incident.ground_truth.onset names no instant", path)
		}
		if g.Onset.ToleranceSeconds <= 0 {
			return fmt.Errorf("fixture: %s: incident.ground_truth.onset.tolerance_seconds must be positive; "+
				"an onset graded to the second is graded on noise", path)
		}
	}
	return g.Provenance.validate(path)
}

func (p EvidencePredicate) validate(field, path string) error {
	if len(p.Term) != 1 {
		return fmt.Errorf("fixture: %s: incident.ground_truth.%s.term names %d families; a predicate is "+
			"about exactly one algebra term", path, field, len(p.Term))
	}
	for family := range p.Term {
		if !slices.Contains(termFamilies, family) {
			return fmt.Errorf("fixture: %s: incident.ground_truth.%s.term names %q, which is not an "+
				"algebra family; want one of %v", path, field, family, termFamilies)
		}
	}
	if strings.TrimSpace(p.FieldPath) == "" {
		return fmt.Errorf("fixture: %s: incident.ground_truth.%s.field_path is required; a predicate over "+
			"a whole digest is a predicate over prose", path, field)
	}
	if !slices.Contains(predicateOps, p.Op) {
		return fmt.Errorf("fixture: %s: incident.ground_truth.%s.op %q is not published; want one of %v",
			path, field, p.Op, predicateOps)
	}
	switch p.Op {
	case "present", "absent":
		// A presence predicate needs no value, and a value on one would be ignored, so it is
		// refused rather than quietly dropped.
		if p.Value != nil {
			return fmt.Errorf("fixture: %s: incident.ground_truth.%s.op is %q and carries a value; "+
				"presence is not a comparison", path, field, p.Op)
		}
	default:
		if p.Value == nil {
			return fmt.Errorf("fixture: %s: incident.ground_truth.%s.op is %q and names no value",
				path, field, p.Op)
		}
	}
	return nil
}

func (p IncidentProvenance) validate(path string) error {
	if !slices.Contains(provenanceKinds, p.Kind) {
		return fmt.Errorf("fixture: %s: incident.ground_truth.provenance.kind %q is not published; want "+
			"one of %v (FR-061b)", path, p.Kind, provenanceKinds)
	}
	if strings.TrimSpace(p.SanitiserPolicyVersion) == "" {
		return fmt.Errorf("fixture: %s: incident.ground_truth.provenance.sanitiser_policy_version is "+
			"required; a fixture whose sanitiser is unnamed cannot be re-derived (FR-061b)", path)
	}
	if p.Kind == "derived" && strings.TrimSpace(p.DerivedFrom) == "" {
		return fmt.Errorf("fixture: %s: a derived fixture must name its parent in "+
			"incident.ground_truth.provenance.derived_from (FR-062a)", path)
	}
	return nil
}

func (w IncidentWorld) validate(path string) error {
	if w.HopRadius <= 0 {
		return fmt.Errorf("fixture: %s: incident.world.hop_radius must be positive", path)
	}
	if w.DrillDownDepth < 0 {
		return fmt.Errorf("fixture: %s: incident.world.drill_down_depth cannot be negative", path)
	}
	if len(w.WindowGrid) == 0 {
		return fmt.Errorf("fixture: %s: incident.world.window_grid is empty; a world with no window grid "+
			"holds no comparisons", path)
	}
	for idx, window := range w.WindowGrid {
		if window.ReferenceAt.IsZero() {
			return fmt.Errorf("fixture: %s: incident.world.window_grid[%d] names no reference_at; the "+
				"grid is centred on the estimated onset, and a grid that does not say where it is "+
				"centred cannot be reviewed", path, idx)
		}
		if window.WidthSeconds <= 0 {
			return fmt.Errorf("fixture: %s: incident.world.window_grid[%d].width_seconds must be positive",
				path, idx)
		}
	}
	if strings.TrimSpace(w.AlgebraVersion) == "" {
		return fmt.Errorf("fixture: %s: incident.world.algebra_version is required; term keys move under "+
			"a different version, so a world that does not state one cannot be replayed", path)
	}
	if w.MissRateThreshold <= 0 || w.MissRateThreshold > 1 {
		return fmt.Errorf("fixture: %s: incident.world.miss_rate_threshold must be in (0, 1]; it is what "+
			"gates the fixture", path)
	}
	if name := strings.TrimSpace(w.Shape); name != "" {
		if _, ok := synthetic.ShapeByName(name); !ok {
			return fmt.Errorf("fixture: %s: incident.world.shape %q is not published; want one of %s",
				path, name, strings.Join(synthetic.ShapeNames(), ", "))
		}
	}
	return nil
}

// ShapeName is the degradation shape this world was recorded at, defaulted. A manifest that names
// none gets `step`, and the default is stated in one place rather than re-defaulted per caller.
func (w IncidentWorld) ShapeName() string {
	if name := strings.TrimSpace(w.Shape); name != "" {
		return name
	}
	return synthetic.ShapeStep
}

func sortedEvidenceKeys(m map[string][]EvidencePredicate) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------- the recorded world, as the verifier reads it ----------

// WorldIndex is as much of `world/index.json` as verification needs. The file is written by
// pkg/backend's recorder; this is a read-only view of it, so that the verifier can hold a
// fixture to the shape its own manifest declares without importing the recorder.
type WorldIndex struct {
	AlgebraVersion   string            `json:"algebraVersion"`
	HopRadius        uint32            `json:"hopRadius"`
	DrillDownDepth   uint32            `json:"drillDownDepth"`
	TermKeyToFile    map[string]string `json:"termKeyToFile"`
	TermCount        uint32            `json:"termCount"`
	NotRecordedCount uint32            `json:"notRecordedCount"`
	MissRate         float64           `json:"missRate"`
	Focus            string            `json:"focus"`
	IndexDigest      string            `json:"indexDigest"`
}

// ReadWorldIndex reads dir/world/index.json. A fixture with no world is not an error: it
// reports ok false, and the caller decides whether that fixture needed one.
func ReadWorldIndex(dir string) (index *WorldIndex, ok bool, err error) {
	path := filepath.Join(dir, "world", "index.json")
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the fixture directory.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("fixture: read %s: %w", path, err)
	}
	index = &WorldIndex{}
	if err := json.Unmarshal(raw, index); err != nil {
		return nil, false, fmt.Errorf("fixture: parse %s: %w", path, err)
	}
	return index, true, nil
}

// TrajectoryFiles lists dir/trajectories/*.jsonl, sorted. An absent directory yields no files
// and no error: trajectories are recorded from the first live run (T093), so every fixture is
// without them until then, and a verifier that treated that as a failure would make the corpus
// unverifiable for the whole of the phase that builds it.
func TrajectoryFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "trajectories"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixture: read trajectories of %s: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		files = append(files, filepath.Join(dir, "trajectories", entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}
