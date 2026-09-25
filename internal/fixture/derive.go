// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The metamorphic generator (T107; FR-062a, SC-020, contracts/incident-format.md §Derived
// fixtures).
//
// **A generator, not checked-in files.** A corpus multiplied by four and committed is a corpus
// four times as expensive to review and no more informative: every variant's events, world and
// goldens would sit in the repository, and the one thing that makes a variant worth having — the
// *relation* between its answer and its parent's — would live nowhere except in a reviewer's
// memory. Here the relation is part of the transform's definition (`internal/eval`'s
// `CheckInvariance`), the variant is regenerated on demand, and what is committed is the rule.
//
// Four transforms, and what each one is for:
//
//   - **culprit-deleted** — the culprit's events are removed. It is the only transform that
//     changes the expected answer: the ground truth becomes `unobserved` with the same symptoms,
//     and a run that names one of the surviving decoys fails. It is the test that the engine can
//     say "nothing I can see explains this" rather than always naming its best candidate.
//   - **decoy-injected** — one more plausible change, inside the window, on an entity beside the
//     one that broke, declared a `coincident` decoy with an exonerating predicate **the
//     regenerated world can actually answer**. A predicate the world cannot answer would fail
//     every run for a reason that has nothing to do with the engine.
//   - **time-shifted** — every instant moves by a fixed offset. Nothing causal changes, so an
//     engine whose answer moves is reading absolute time somewhere it should be reading
//     durations.
//   - **name-permuted** — a deterministic, namespace-preserving bijection over the entity names,
//     seeded from the parent's id, applied to events, manifest, culprit and every decoy through
//     the same map. This is the one that tests what the corpus cannot otherwise test: that the
//     engine ranks on **structure** rather than on guilty-sounding names. `payments` is the
//     obvious culprit to a reader; after the permutation it is called something else and the
//     structure is all that is left.
//
// Two properties the generator has to have, and both are tested:
//
//  1. **Determinism.** Deriving twice yields identical bytes — the same events, the same manifest,
//     the same world, the same goldens. A generator that did not would make every metamorphic
//     failure unreproducible.
//  2. **`world/` is regenerated, never copied.** A copied world would answer with the parent's
//     names and the parent's instants, so a name-permuted variant would ask about `svc-a` and be
//     told about `payments`, and every answer would be `not_recorded`. The world is a function of
//     the graph, and the graph is what the transform changed.

// The four published transforms, spelled as `fixture derive --transform` takes them. They are
// `internal/eval`'s constants too; the two sets are asserted equal by that package's tests, and
// they are declared twice because `internal/eval` sits above this package and may not be imported
// by it.
const (
	TransformCulpritDeleted = "culprit-deleted"
	TransformDecoyInjected  = "decoy-injected"
	TransformTimeShifted    = "time-shifted"
	TransformNamePermuted   = "name-permuted"
)

// Transforms is the published set, in the order the contract lists them.
var Transforms = []string{
	TransformCulpritDeleted, TransformDecoyInjected, TransformTimeShifted, TransformNamePermuted,
}

// DefaultTimeShift is how far `time-shifted` moves a fixture: one week.
//
// A whole number of days, on purpose. The onset estimator's published method is a seasonal CUSUM
// with a 86 400-second period, so an offset that is not a multiple of a day would move the
// symptom relative to the season and the transform would no longer be causally neutral — it would
// be testing seasonality, which is a different question and not one the invariant states.
const DefaultTimeShift = 7 * 24 * time.Hour

// DeriveOptions is how a variant is asked for.
type DeriveOptions struct {
	// Transform is one of Transforms.
	Transform string
	// Out is the variant's directory. Its base name becomes the variant's id, because 001's rule
	// is that a fixture's id equals its directory name and a derived fixture is a fixture.
	Out string
	// Offset is `time-shifted`'s offset. Zero means DefaultTimeShift.
	Offset time.Duration
	// Seed varies `name-permuted`'s bijection. Empty means the parent's id, so the same parent
	// always yields the same permutation and a failure is reproducible by name.
	Seed string
	// Force overwrites an existing output directory. Without it a non-empty directory is
	// refused, because silently rewriting one is how a stale world survives a re-derivation.
	Force bool
}

// DeriveReport is what a derivation produced.
type DeriveReport struct {
	// ParentID and VariantID name the two fixtures, and Transform the relation between them.
	ParentID  string `json:"parent_id"`
	VariantID string `json:"variant_id"`
	Transform string `json:"transform"`
	// Dir is where the variant was written. It is deliberately **not** serialised: the file this
	// report is written into lives in that directory, and an absolute path baked into a
	// generated artefact is the one thing that would make two derivations of the same variant
	// differ byte for byte on two machines.
	Dir string `json:"-"`
	// Events is how many events the variant carries, Removed how many the transform deleted and
	// Added how many it injected.
	Events  int `json:"events"`
	Removed int `json:"removed"`
	Added   int `json:"added"`
	// Culprit is the variant's ground-truth culprit: the transform's published image of the
	// parent's.
	Culprit string `json:"culprit"`
	// Offset is the instant shift applied, for `time-shifted`.
	Offset time.Duration `json:"offset,omitempty"`
	// NameMap is `name-permuted`'s bijection, parent name to image. It is what the invariance
	// check maps the parent's verdict through, so it is published rather than re-derived.
	NameMap map[string]string `json:"name_map,omitempty"`
	// InjectedDecoy is the change `decoy-injected` added.
	InjectedDecoy string `json:"injected_decoy,omitempty"`
	// Files lists what was written, relative to Dir, sorted.
	Files []string `json:"files"`
	// WorldPending and GoldensPending say the variant is not yet complete: a world has to be
	// recorded against its regenerated graph and its goldens re-recorded. They are always true —
	// this package cannot record either, because both need a database and the engine, which sit
	// above it — and they are published so a caller that skipped those steps cannot believe it
	// has a verifiable fixture.
	WorldPending   bool `json:"world_pending"`
	GoldensPending bool `json:"goldens_pending"`
}

// The published refusal reasons of `fixture derive`. A refusal is a statement that this parent
// cannot carry this transform — not that the run went wrong — and a caller that wants to skip a
// parent rather than fail a batch needs to tell the two apart without reading prose.
const (
	// ReasonUnpublishedTransform is a transform this package does not publish.
	ReasonUnpublishedTransform = "unpublished_transform"
	// ReasonNotAnIncident is a parent with no `incident:` block: there is no ground truth to take
	// an image of.
	ReasonNotAnIncident = "not_an_incident"
	// ReasonNoOutput is a derivation with nowhere to write.
	ReasonNoOutput = "no_output"
	// ReasonOutputNotEmpty is an output directory that already holds something, without --force.
	ReasonOutputNotEmpty = "output_not_empty"
	// ReasonUnusableGroundTruth is a manifest whose ground truth the transform cannot act on.
	ReasonUnusableGroundTruth = "unusable_ground_truth"
	// ReasonNothingToTransform is a graph that holds nothing for this transform to move: no event
	// naming the culprit, no second workload to build a decoy on, fewer than two names to permute.
	ReasonNothingToTransform = "nothing_to_transform"
)

// Refusal is a typed refusal from Derive. Its message is the sentence and nothing else — the
// reason code is carried beside it rather than prefixed onto it, so that a published refusal
// reads the same to a person whether or not the caller matched on the code.
type Refusal struct {
	// Reason is one of the published codes above.
	Reason string
	// Detail is the sentence.
	Detail string
}

// Error implements error.
func (r *Refusal) Error() string { return r.Detail }

// Refuse builds a Refusal with a formatted detail.
func Refuse(reason, format string, args ...any) error {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// RefusalReason returns the published reason an error carries, or "" for any other error.
func RefusalReason(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// Derive writes the variant of the fixture in dir that the transform defines.
//
// It writes the manifest, the events and the payloads. It does **not** write `world/` or
// `golden/`: both are functions of the regenerated graph, both need a database and the recorder,
// and both live above this package. The report says so, and `fixture derive` runs them.
func Derive(dir string, opts DeriveOptions) (*DeriveReport, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	if !m.IsIncident() {
		return nil, Refuse(ReasonNotAnIncident,
			"fixture: derive %s: the parent has no `incident:` block; a variant is graded on the image "+
				"of its parent's ground truth and there is none to take an image of", dir)
	}
	if !contains(Transforms, opts.Transform) {
		return nil, Refuse(ReasonUnpublishedTransform,
			"fixture: derive: --transform %q is not published; want one of %s",
			opts.Transform, strings.Join(Transforms, ", "))
	}
	if strings.TrimSpace(opts.Out) == "" {
		return nil, Refuse(ReasonNoOutput, "fixture: derive: --out names no directory to write the variant into")
	}
	if opts.Offset == 0 {
		opts.Offset = DefaultTimeShift
	}
	if opts.Seed == "" {
		opts.Seed = m.ID
	}

	out := filepath.Clean(opts.Out)
	variantID := filepath.Base(out)
	created, err := prepareOutput(out, opts.Force)
	if err != nil {
		return nil, err
	}
	// A refusal leaves nothing behind. `fixture derive` creates the output directory before it
	// knows whether the transform applies to this parent — it cannot know earlier, because the
	// answer is in the parent's events — and an empty directory left where a variant was refused
	// is a directory the next run finds non-empty, refuses again for a different reason, and
	// makes a reviewer delete by hand. Only a directory *this* run created is removed: a --force
	// derivation over somebody's existing variant must not take it with it when it refuses.
	abandon := func(err error) (*DeriveReport, error) {
		if created {
			_ = os.RemoveAll(out)
		}
		return nil, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // the fixture the caller named
	if err != nil {
		return abandon(fmt.Errorf("fixture: derive: read the parent manifest: %w", err))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return abandon(fmt.Errorf("fixture: derive: parse the parent manifest: %w", err))
	}
	root := documentRoot(&doc)
	if root == nil {
		return abandon(fmt.Errorf("fixture: derive: the parent manifest is not a mapping"))
	}

	events, err := readRawEvents(m.EventsPath())
	if err != nil {
		return abandon(err)
	}

	report := &DeriveReport{
		ParentID: m.ID, VariantID: variantID, Transform: opts.Transform, Dir: out,
		WorldPending: true, GoldensPending: true,
	}

	switch opts.Transform {
	case TransformCulpritDeleted:
		events, err = deleteCulprit(m, root, events, report)
	case TransformDecoyInjected:
		events, err = injectDecoy(m, root, events, report)
	case TransformTimeShifted:
		events, err = shiftTime(root, events, opts.Offset, report)
	case TransformNamePermuted:
		events, err = permuteNames(m, root, events, opts.Seed, report)
	}
	if err != nil {
		return abandon(err)
	}

	setScalar(root, variantID, "id")
	describeVariant(root, m, report, opts)
	stampProvenance(root, m, report, opts)

	report.Events = len(events)
	report.Culprit = scalarAt(root, "incident", "ground_truth", "culprit")
	if err := writeVariant(dir, out, m, root, events, report); err != nil {
		return abandon(err)
	}
	return report, nil
}

// prepareOutput refuses to write over a directory that already holds something, unless told to.
// It reports whether it created the directory, so that a refusal further down can remove what it
// made and nothing else.
func prepareOutput(out string, force bool) (bool, error) {
	entries, err := os.ReadDir(out)
	switch {
	case os.IsNotExist(err):
		return true, os.MkdirAll(out, 0o750)
	case err != nil:
		return false, fmt.Errorf("fixture: derive: read %s: %w", out, err)
	case len(entries) > 0 && !force:
		return false, Refuse(ReasonOutputNotEmpty,
			"fixture: derive: %s is not empty; pass --force to replace it. A variant written over an "+
				"older one keeps that one's `world/` and `golden/`, and a world recorded from a different "+
				"graph answers every question with the wrong fixture's numbers", out)
	}
	// Force: the two generated directories go, because they are functions of the graph this run
	// is about to replace. Everything else is overwritten file by file.
	for _, name := range []string{"world", GoldenDir} {
		if err := os.RemoveAll(filepath.Join(out, name)); err != nil {
			return false, fmt.Errorf("fixture: derive: clear %s: %w", filepath.Join(out, name), err)
		}
	}
	return false, nil
}

// writeVariant writes the manifest, the events and the payloads.
func writeVariant(parentDir, out string, m *Manifest, root *yaml.Node, events []rawEvent, report *DeriveReport) error {
	manifest, err := renderYAML(root)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(out, ManifestFile), manifest, report, out); err != nil {
		return err
	}

	body, err := renderRawEvents(events)
	if err != nil {
		return err
	}
	eventsName := m.Events
	if eventsName == "" {
		eventsName = DefaultEventsFile
	}
	if err := writeFile(filepath.Join(out, eventsName), body, report, out); err != nil {
		return err
	}

	// The rejected-event file, where there is one, travels with the fixture: a variant that lost
	// its refusals would verify one check fewer and nothing would say so.
	if m.RejectedEvents != "" {
		rejected, err := readRawEvents(filepath.Join(parentDir, m.RejectedEvents))
		if err != nil {
			return err
		}
		rendered, err := renderRawEvents(rejected)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(out, m.RejectedEvents), rendered, report, out); err != nil {
			return err
		}
	}

	// payloads/ is the fixture's source material. It is copied verbatim: the transforms above
	// rewrite the *events*, which is what the graph is built from, and a payload rewritten by a
	// different code path would be a second, drifting derivation of the same fixture.
	payloads := filepath.Join(parentDir, "payloads")
	if info, err := os.Stat(payloads); err == nil && info.IsDir() {
		if err := copyTree(payloads, filepath.Join(out, "payloads"), report, out); err != nil {
			return err
		}
	}
	sort.Strings(report.Files)
	return WriteDeriveReport(report)
}

// DeriveReportFile is where a variant records how it was derived.
//
// It is a file beside the manifest rather than a key inside it for two reasons. The manifest's
// decoder is strict, so every key it may carry is a key `internal/fixture` has to publish, and a
// generator's bookkeeping is not part of the incident format. And the one thing in here that a
// checker needs — `name-permuted`'s bijection — must be *read* rather than re-derived: a checker
// that recomputed the permutation would be comparing the engine's answer against its own
// recomputation, which agrees with itself by construction.
const DeriveReportFile = "derived.json"

// WriteDeriveReport writes the report into the variant's directory.
func WriteDeriveReport(report *DeriveReport) error {
	body, err := graph.CanonicalJSON(*report)
	if err != nil {
		return fmt.Errorf("fixture: derive: render %s: %w", DeriveReportFile, err)
	}
	path := filepath.Join(report.Dir, DeriveReportFile)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("fixture: derive: write %s: %w", path, err)
	}
	if !contains(report.Files, DeriveReportFile) {
		report.Files = append(report.Files, DeriveReportFile)
		sort.Strings(report.Files)
	}
	return nil
}

// ReadDeriveReport reads a variant's own account of how it was derived. A directory with no such
// file is not a derived fixture, which the second return value says.
func ReadDeriveReport(dir string) (*DeriveReport, bool, error) {
	body, err := os.ReadFile(filepath.Join(dir, DeriveReportFile)) //nolint:gosec // the fixture the caller named
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("fixture: read %s: %w", filepath.Join(dir, DeriveReportFile), err)
	}
	report := &DeriveReport{}
	if err := json.Unmarshal(body, report); err != nil {
		return nil, false, fmt.Errorf("fixture: parse %s: %w", filepath.Join(dir, DeriveReportFile), err)
	}
	return report, true, nil
}

func writeFile(path string, body []byte, report *DeriveReport, out string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("fixture: derive: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("fixture: derive: write %s: %w", path, err)
	}
	if relative, err := filepath.Rel(out, path); err == nil {
		report.Files = append(report.Files, filepath.ToSlash(relative))
	}
	return nil
}

func copyTree(from, to string, report *DeriveReport, out string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // inside the parent fixture
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		return writeFile(filepath.Join(to, relative), body, report, out)
	})
}

// ---------- the transforms ----------

// deleteCulprit removes the culprit's events and takes the ground truth to `unobserved`.
func deleteCulprit(m *Manifest, root *yaml.Node, events []rawEvent, report *DeriveReport) ([]rawEvent, error) {
	culprit := strings.TrimSpace(m.Incident.GroundTruth.Culprit)
	ref, err := graph.ParseRef(culprit)
	if err != nil {
		return nil, Refuse(ReasonUnusableGroundTruth,
			"fixture: derive %s: the parent's culprit %q is not a change identifier, so there is nothing "+
				"to delete (a fixture whose truth is already `unobserved` has no culprit-deleted variant)",
			m.ID, culprit)
	}

	kept := make([]rawEvent, 0, len(events))
	for _, event := range events {
		if namesRef(event.body, ref.Namespace, ref.Value) {
			report.Removed++
			continue
		}
		kept = append(kept, event)
	}
	if report.Removed == 0 {
		return nil, Refuse(ReasonNothingToTransform,
			"fixture: derive %s: no event names the culprit %s, so deleting it would change nothing and "+
				"the variant would be its parent under another name", m.ID, culprit)
	}
	renumber(kept)

	truth := mapAt(root, "incident", "ground_truth")
	if truth == nil {
		return nil, Refuse(ReasonUnusableGroundTruth, "fixture: derive %s: the manifest has no incident.ground_truth", m.ID)
	}
	setScalar(truth, CulpritUnobserved, "culprit")

	// The causal path keeps the symptom and loses the cause: the symptom is still real and still
	// localised, which is exactly what SC-023 grades, and the change that explained it is gone.
	if path := seqAt(truth, "causal_path"); path != nil && len(path.Content) > 1 {
		if scalarOf(path.Content[0], "entity") == culprit {
			path.Content = path.Content[1:]
			last := path.Content[len(path.Content)-1]
			deleteKey(last, "via")
			setScalar(last, "symptom", "role")
		}
	}

	// The decisive evidence becomes what settles the question now: every remaining candidate is
	// ruled out. Those are the parent's own exonerating predicates, which the regenerated world
	// still answers — the candidates were innocent in the parent and nothing this transform does
	// makes them guilty.
	if err := decisiveFromExonerations(truth); err != nil {
		return nil, fmt.Errorf("fixture: derive %s: %w", m.ID, err)
	}
	return kept, nil
}

// decisiveFromExonerations replaces `decisive_evidence` with the union of the exonerating
// predicates, sorted by the decoy they belong to so the result is a function of the parent.
func decisiveFromExonerations(truth *yaml.Node) error {
	exonerating := mapAt(truth, "exonerating_evidence")
	if exonerating == nil {
		return fmt.Errorf("the parent declares no exonerating_evidence, so a culprit-deleted variant " +
			"would have nothing decisive to say about why nothing explains the symptom")
	}
	union := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for i := 0; i+1 < len(exonerating.Content); i += 2 {
		predicates := exonerating.Content[i+1]
		if predicates.Kind != yaml.SequenceNode {
			continue
		}
		union.Content = append(union.Content, predicates.Content...)
	}
	if len(union.Content) == 0 {
		return fmt.Errorf("every decoy is exonerated by nothing, so a culprit-deleted variant states " +
			"no decisive evidence and could not be graded")
	}
	setNode(truth, union, "decisive_evidence")
	return nil
}

// injectDecoy adds one more plausible change inside the window, on an entity beside the one that
// broke, and declares it.
func injectDecoy(m *Manifest, root *yaml.Node, events []rawEvent, report *DeriveReport) ([]rawEvent, error) {
	target, err := adjacentTarget(m, events)
	if err != nil {
		return nil, err
	}
	service := lastSegment(target)
	firedAt := m.Incident.Question.FiredAt.UTC()
	validAt := firedAt.Add(-3 * time.Minute)
	observedAt := validAt.Add(30 * time.Second)

	changeValue := target + "@derived-decoy"
	report.InjectedDecoy = "k8s.change=" + changeValue
	eventID := "k8s:demo:change:" + changeValue

	schema := m.SchemaVersion
	if schema == "" {
		schema = "1.0.0"
	}
	source := "k8s:demo"
	if len(m.Sources) > 0 {
		source = m.Sources[0].SourceID
	}
	body := map[string]any{
		"eventId":        eventID,
		"idempotencyKey": eventID,
		"observedAt":     observedAt.Format(time.RFC3339),
		"schemaVersion":  schema,
		"sourceId":       source,
		"observeChange": map[string]any{
			"change": map[string]any{
				"actor":     "deploy-bot",
				"kind":      "ROLLOUT",
				"originRef": "https://github.com/" + target + "/actions/runs/derived-decoy",
				"summary":   "rollout " + service + " (injected by the decoy-injected transform)",
			},
			"ref":     map[string]any{"namespace": "k8s.change", "value": changeValue},
			"targets": []any{map[string]any{"namespace": "k8s.deployment", "value": target}},
			"validAt": validAt.Format(time.RFC3339),
		},
	}
	events = append(events, rawEvent{body: body, observedAt: observedAt})
	sortByObservedAt(events)
	renumber(events)
	report.Added = 1

	truth := mapAt(root, "incident", "ground_truth")
	if truth == nil {
		return nil, Refuse(ReasonUnusableGroundTruth, "fixture: derive %s: the manifest has no incident.ground_truth", m.ID)
	}
	decoy, err := node(map[string]any{
		"entity":      report.InjectedDecoy,
		"causal_role": "coincident",
		"why": fmt.Sprintf(
			"injected by the %s transform: a rollout of %s three minutes before the alert, one hop from "+
				"the subject and inside the window, which is the shape a ranker rewards. It is innocent, "+
				"and %s's own error rate does not move across the onset.",
			TransformDecoyInjected, service, service),
	})
	if err != nil {
		return nil, err
	}
	appendTo(truth, decoy, "decoys")

	// The exonerating predicate has to be one the regenerated world can answer, so it is written
	// over the manifest's own window grid rather than over an interval invented here: the grid is
	// what `fixture record-world` records the cross product over.
	window := m.Incident.World.WindowGrid[0]
	predicate, err := node([]any{map[string]any{
		"term": map[string]any{"compare": map[string]any{
			"pointer":   "METRIC otel.service.name=" + service,
			"windows":   map[string]any{"reference_at": window.ReferenceAt.UTC(), "width_seconds": window.WidthSeconds},
			"statistic": "ERROR_RATE",
		}},
		"field_path": "comparisons[statistic=ERROR_RATE].direction",
		"op":         "eq",
		"value":      "flat",
		"note": fmt.Sprintf(
			"%s's error rate does not move across the onset; the injected change is coincident with the "+
				"symptom and causes none of it.", service),
	}})
	if err != nil {
		return nil, err
	}
	exonerating := mapAt(truth, "exonerating_evidence")
	if exonerating == nil {
		return nil, Refuse(ReasonNothingToTransform,
			"fixture: derive %s: the manifest has no exonerating_evidence to add to", m.ID)
	}
	exonerating.Content = append(exonerating.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: report.InjectedDecoy}, predicate)
	return events, nil
}

// adjacentTarget picks the entity the injected change lands on.
//
// The rule, stated so a reader can predict it: a `k8s.deployment` the graph already describes,
// that no change has touched, that is not the subject's own workload, chosen in sorted order. It
// is not the subject's own because a change on the thing that broke is a plausible *cause*, not a
// decoy, and injecting one would be testing whether the engine can be fooled by a true positive.
func adjacentTarget(m *Manifest, events []rawEvent) (string, error) {
	deployments := map[string]struct{}{}
	changed := map[string]struct{}{}
	for _, event := range events {
		if node, ok := event.body["upsertNode"].(map[string]any); ok {
			if ref, ok := node["ref"].(map[string]any); ok && asText(ref["namespace"]) == "k8s.deployment" {
				deployments[asText(ref["value"])] = struct{}{}
			}
		}
		if change, ok := event.body["observeChange"].(map[string]any); ok {
			targets, _ := change["targets"].([]any)
			for _, target := range targets {
				if ref, ok := target.(map[string]any); ok {
					changed[asText(ref["value"])] = struct{}{}
				}
			}
		}
	}
	subject := lastSegment(m.Incident.Question.Subject)
	culpritTarget := lastSegment(m.Incident.GroundTruth.Culprit)

	var untouched, alreadyChanged, own []string
	for value := range deployments {
		switch {
		case lastSegment(value) == culpritTarget:
			// Never the culprit's own workload: a second change there is a plausible *cause*,
			// and injecting one would be testing whether the engine can be fooled by a true
			// positive rather than by a decoy.
		case lastSegment(value) == subject:
			own = append(own, value)
		default:
			if _, taken := changed[value]; taken {
				alreadyChanged = append(alreadyChanged, value)
			} else {
				untouched = append(untouched, value)
			}
		}
	}
	// The preference order, stated so a reader can predict which workload a transform picks: a
	// workload the graph describes and no change has touched; failing that, one that already
	// carries a change, because a second rollout of the same service inside the window is a
	// perfectly ordinary thing and is still coincident; failing that, the subject's own workload,
	// which is the last resort and is still not the culprit's.
	for _, group := range [][]string{untouched, alreadyChanged, own} {
		if len(group) > 0 {
			sort.Strings(group)
			return group[0], nil
		}
	}
	return "", Refuse(ReasonNothingToTransform,
		"fixture: derive %s: the graph describes no workload other than the culprit's own, so there is "+
			"nowhere to inject a change that would be a decoy rather than a second candidate cause", m.ID)
}

// shiftTime moves every instant by a fixed offset.
//
// Every instant means every one: the events' valid and observed times, the manifest's clock, its
// queries, the question, the knowability instant, the onset, the window grid and the instants
// inside the evidence predicates. The rewrite is by *shape* — any scalar that parses as RFC 3339
// is an instant — rather than by a list of keys, because a list of keys is a list somebody has to
// remember to extend and an instant left behind is a fixture whose world and whose question are
// a week apart.
func shiftTime(root *yaml.Node, events []rawEvent, offset time.Duration, report *DeriveReport) ([]rawEvent, error) {
	report.Offset = offset
	for i := range events {
		events[i].body = shiftValue(events[i].body, offset).(map[string]any)
		if at, ok := parseInstant(asText(events[i].body["observedAt"])); ok {
			events[i].observedAt = at
		}
	}
	sortByObservedAt(events)
	renumber(events)

	walkScalars(root, func(scalar *yaml.Node) {
		if at, ok := parseInstant(scalar.Value); ok {
			scalar.Value = at.Add(offset).UTC().Format(time.RFC3339)
			return
		}
		// A window written as `<start>..<end>`, which is how the evidence predicates spell one.
		if start, end, found := strings.Cut(scalar.Value, ".."); found {
			from, okFrom := parseInstant(strings.TrimSpace(start))
			to, okTo := parseInstant(strings.TrimSpace(end))
			if okFrom && okTo {
				scalar.Value = from.Add(offset).UTC().Format(time.RFC3339) + ".." +
					to.Add(offset).UTC().Format(time.RFC3339)
			}
		}
	})
	return events, nil
}

// shiftValue walks a decoded JSON value and shifts every instant in it.
func shiftValue(value any, offset time.Duration) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, each := range typed {
			typed[key] = shiftValue(each, offset)
		}
		return typed
	case []any:
		for i, each := range typed {
			typed[i] = shiftValue(each, offset)
		}
		return typed
	case string:
		if at, ok := parseInstant(typed); ok {
			return at.Add(offset).UTC().Format(time.RFC3339)
		}
		return typed
	default:
		return value
	}
}

// permuteNames applies a deterministic, namespace-preserving bijection over the entity names.
//
// The bijection is a **derangement of the names the fixture already uses**, not a mapping onto
// invented ones. That is the sharper test: after it, `payments` is called `inventory` and
// `inventory` is called something else, so a ranker that had learned which name sounds guilty
// now has a name that sounds guilty attached to an innocent structure. Inventing opaque names
// would also work against a ranker that memorised names, but not against one that had learned
// "the shorter, more infrastructural-sounding name is usually the cause".
//
// It is applied as a whole-token text substitution over the events and the manifest, because a
// name appears in far more places than a ref: in pointer selectors, in display names, in resource
// attributes, in a change's summary. A rewrite that only touched refs would leave a graph whose
// pointers still selected the parent's series.
func permuteNames(m *Manifest, root *yaml.Node, events []rawEvent, seed string, report *DeriveReport) ([]rawEvent, error) {
	names := permutableNames(events)
	if len(names) < 2 {
		return nil, Refuse(ReasonNothingToTransform,
			"fixture: derive %s: the graph names %d entity/entities, and a permutation over fewer than two "+
				"is the identity", m.ID, len(names))
	}
	mapping := derange(names, seed)
	report.NameMap = mapping

	for i := range events {
		body, err := json.Marshal(events[i].body)
		if err != nil {
			return nil, fmt.Errorf("fixture: derive %s: re-encode an event: %w", m.ID, err)
		}
		var rewritten map[string]any
		decoder := json.NewDecoder(bytes.NewReader([]byte(substitute(string(body), mapping))))
		decoder.UseNumber()
		if err := decoder.Decode(&rewritten); err != nil {
			return nil, fmt.Errorf("fixture: derive %s: re-decode an event: %w", m.ID, err)
		}
		events[i].body = rewritten
	}
	walkScalars(root, func(scalar *yaml.Node) {
		scalar.Value = substitute(scalar.Value, mapping)
	})
	return events, nil
}

// permutableNames collects the names a permutation may move: the **last** `/`-separated segment
// of every ref value the events name, with any `@revision` suffix left alone.
//
// Two things are deliberately left alone, and both are what "namespace-preserving" means here.
// The path prefix — `shop/` in `k8s.deployment=shop/payments`, `shop-prod/` in a node pool — is
// the namespace the entity lives in, and permuting it would move entities between namespaces
// rather than rename them, which is a structural change and not a renaming. The `@revision`
// suffix names a version rather than an entity; permuting it would change nothing structural and
// would make the diff unreadable.
func permutableNames(events []rawEvent) []string {
	seen := map[string]struct{}{}
	var collect func(value any)
	collect = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if namespace, ok := typed["namespace"].(string); ok && namespace != "" {
				if raw, ok := typed["value"].(string); ok {
					base, _, _ := strings.Cut(raw, "@")
					if segment := base[strings.LastIndex(base, "/")+1:]; segment != "" {
						seen[segment] = struct{}{}
					}
				}
			}
			for _, each := range typed {
				collect(each)
			}
		case []any:
			for _, each := range typed {
				collect(each)
			}
		}
	}
	for _, event := range events {
		collect(event.body)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// derange builds a deterministic permutation of names in which no name maps to itself.
//
// It is a seeded rotation over a seeded shuffle: the shuffle makes the pairing unpredictable from
// the sorted order, and the rotation guarantees the derangement property without a rejection loop
// that could, in principle, not terminate.
func derange(names []string, seed string) map[string]string {
	shuffled := append([]string(nil), names...)
	sum := sha256.Sum256([]byte(seed))
	state := binary.BigEndian.Uint64(sum[:8]) | 1
	next := func() uint64 {
		// xorshift64*: deterministic, tiny, and its only requirement here is that the same seed
		// gives the same order on every machine and every Go version. `math/rand`'s sequence is
		// not contractually stable across versions, and a permutation that changed under a
		// toolchain upgrade would silently re-derive every variant.
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		return state * 2685821657736338717
	}
	for i := len(shuffled) - 1; i > 0; i-- {
		j := int(next() % uint64(i+1)) //nolint:gosec // bounded by i+1
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	out := make(map[string]string, len(shuffled))
	for i, name := range shuffled {
		out[name] = shuffled[(i+1)%len(shuffled)]
	}
	return out
}

// substitute applies the mapping to whole tokens of text, in two passes so that a name mapped
// onto another name in the set cannot be substituted twice.
func substitute(text string, mapping map[string]string) string {
	if text == "" {
		return text
	}
	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	// Longest first, so a name that is a prefix of another cannot claim it. The token-boundary
	// test below already prevents that; the ordering makes the result independent of map order
	// for any future boundary rule.
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	placeholders := make(map[string]string, len(names))
	for i, name := range names {
		placeholders[name] = fmt.Sprintf("\x00%d\x00", i)
		text = replaceTokens(text, name, placeholders[name])
	}
	for _, name := range names {
		text = strings.ReplaceAll(text, placeholders[name], mapping[name])
	}
	return text
}

// replaceTokens replaces every whole-token occurrence of name. A token character is a letter, a
// digit, `_` or `-`, so `checkout` never matches inside `checkout-worker`.
func replaceTokens(text, name, with string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if !strings.HasPrefix(text[i:], name) {
			b.WriteByte(text[i])
			i++
			continue
		}
		beforeOK := i == 0 || !isTokenByte(text[i-1])
		after := i + len(name)
		afterOK := after >= len(text) || !isTokenByte(text[after])
		if beforeOK && afterOK {
			b.WriteString(with)
			i = after
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func isTokenByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ---------- the manifest's own bookkeeping ----------

// describeVariant rewrites the description so a reader opening the directory knows in one
// paragraph what it is and that they should not edit it.
func describeVariant(root *yaml.Node, m *Manifest, report *DeriveReport, opts DeriveOptions) {
	detail := ""
	switch opts.Transform {
	case TransformCulpritDeleted:
		detail = fmt.Sprintf("%d event(s) naming the culprit %s were removed and the ground truth is "+
			"`unobserved` with the same symptoms; naming any surviving candidate is a failure.",
			report.Removed, m.Incident.GroundTruth.Culprit)
	case TransformDecoyInjected:
		detail = fmt.Sprintf("one plausible change, %s, was added inside the window with an exonerating "+
			"predicate the recorded world answers; the verdict must not move.", report.InjectedDecoy)
	case TransformTimeShifted:
		detail = fmt.Sprintf("every instant moved by %s; nothing causal changed, so the verdict must not "+
			"move.", opts.Offset)
	case TransformNamePermuted:
		detail = "every entity name was permuted through a deterministic, namespace-preserving " +
			"bijection seeded from the parent's id; the verdict, mapped through that bijection, must " +
			"not move. The permutation is what tests that the engine ranks on structure rather than " +
			"on guilty-sounding names."
	}
	setScalar(root, fmt.Sprintf(
		"GENERATED — do not edit. The %s variant of %s, written by `aisre fixture derive`. %s\n\n"+
			"It is graded on the invariant its transform defines, which is part of that transform's "+
			"definition rather than a property of this directory; regenerate it rather than editing it.",
		opts.Transform, m.ID, detail), "description")
}

// stampProvenance records the parent and the transformation, which is what makes a derived
// fixture re-derivable (FR-061b, FR-062a).
func stampProvenance(root *yaml.Node, m *Manifest, report *DeriveReport, opts DeriveOptions) {
	provenance := mapAt(root, "incident", "ground_truth", "provenance")
	if provenance == nil {
		return
	}
	setScalar(provenance, "derived", "kind")
	setScalar(provenance, m.ID, "derived_from")
	transformation := opts.Transform
	switch opts.Transform {
	case TransformTimeShifted:
		transformation += fmt.Sprintf("; offset %s", opts.Offset)
	case TransformNamePermuted:
		transformation += fmt.Sprintf("; seed %q, %d names permuted", opts.Seed, len(report.NameMap))
	case TransformCulpritDeleted:
		transformation += fmt.Sprintf("; %d event(s) removed", report.Removed)
	case TransformDecoyInjected:
		transformation += fmt.Sprintf("; %s injected", report.InjectedDecoy)
	}
	setScalar(provenance, transformation+
		"; world re-recorded from this directory's graph, goldens re-recorded from this directory",
		"transformation")
	// A generated variant is not hand-authored, whatever its parent said: `fixture record` may
	// rewrite its events, because they came from a generator and not from a person.
	setScalar(root, "false", "hand_authored")
	if flag := mapValue(root, "hand_authored"); flag != nil {
		flag.Tag = "!!bool"
	}
}

// ---------- raw events ----------

// rawEvent is one line of an events file, decoded but not validated. Deriving works on the raw
// shape rather than on `Event` because a transform touches keys the envelope does not model —
// `appendedSeq`, `observedAt` — and because a variant's events are written by re-encoding what
// was read rather than by re-deriving it.
type rawEvent struct {
	body       map[string]any
	observedAt time.Time
}

func readRawEvents(path string) ([]rawEvent, error) {
	body, err := os.ReadFile(path) //nolint:gosec // the fixture the caller named
	if err != nil {
		return nil, fmt.Errorf("fixture: derive: read %s: %w", path, err)
	}
	var out []rawEvent
	for i, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber() // so a large sequence number survives the round trip exactly
		var decoded map[string]any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("fixture: derive: %s line %d: %w", path, i+1, err)
		}
		event := rawEvent{body: decoded}
		if at, ok := parseInstant(asText(decoded[observedAtField])); ok {
			event.observedAt = at
		}
		out = append(out, event)
	}
	return out, nil
}

func renderRawEvents(events []rawEvent) ([]byte, error) {
	var out bytes.Buffer
	for i, event := range events {
		line, err := graph.CanonicalJSON(event.body)
		if err != nil {
			return nil, fmt.Errorf("fixture: derive: encode event %d: %w", i+1, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// renumber rewrites `appendedSeq` so the variant's log is contiguous from 1, as a log is.
func renumber(events []rawEvent) {
	for i := range events {
		events[i].body[appendedSeqField] = json.Number(fmt.Sprintf("%d", i+1))
	}
}

// sortByObservedAt puts the events into observed-time order, stably, which is the order a log
// appended them in.
func sortByObservedAt(events []rawEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].observedAt.Before(events[j].observedAt)
	})
}

// namesRef reports whether an event is about the given ref: its own identity, a change it
// observes, or an edge endpoint.
func namesRef(value any, namespace, name string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if asText(typed["namespace"]) == namespace && asText(typed["value"]) == name {
			return true
		}
		for _, each := range typed {
			if namesRef(each, namespace, name) {
				return true
			}
		}
	case []any:
		for _, each := range typed {
			if namesRef(each, namespace, name) {
				return true
			}
		}
	}
	return false
}

// ---------- small helpers ----------

func parseInstant(value string) (time.Time, bool) {
	if len(value) < len("2006-01-02T15:04:05Z") {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

func asText(value any) string {
	text, _ := value.(string)
	return text
}

func lastSegment(value string) string {
	if _, after, found := strings.Cut(value, "="); found {
		value = after
	}
	if base, _, found := strings.Cut(value, "@"); found {
		value = base
	}
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ---------- yaml.Node navigation ----------
//
// The manifest is edited as a node tree rather than decoded into `Manifest` and re-encoded, for
// one reason a reviewer feels immediately: the comments. A fixture manifest in this corpus is
// half prose — the `note:` explaining why a window grid is centred where it is, the `why:` on
// every decoy — and a round trip through a struct would silently delete all of it, leaving a
// generated fixture that nobody could review.

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	return doc
}

// mapValue returns the value node for one key of a mapping.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapAt walks a path of mapping keys.
func mapAt(root *yaml.Node, path ...string) *yaml.Node {
	current := root
	for _, key := range path {
		current = mapValue(current, key)
		if current == nil {
			return nil
		}
	}
	if current.Kind != yaml.MappingNode {
		return nil
	}
	return current
}

func seqAt(root *yaml.Node, path ...string) *yaml.Node {
	current := root
	for _, key := range path {
		current = mapValue(current, key)
		if current == nil {
			return nil
		}
	}
	if current.Kind != yaml.SequenceNode {
		return nil
	}
	return current
}

func scalarAt(root *yaml.Node, path ...string) string {
	current := root
	for _, key := range path {
		current = mapValue(current, key)
		if current == nil {
			return ""
		}
	}
	return current.Value
}

func scalarOf(m *yaml.Node, key string) string {
	if value := mapValue(m, key); value != nil {
		return value.Value
	}
	return ""
}

// setScalar sets a scalar value at a path, creating the key when it is absent.
func setScalar(root *yaml.Node, value string, path ...string) {
	setNode(root, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}, path...)
}

// setNode replaces (or creates) the value at a path.
func setNode(root *yaml.Node, value *yaml.Node, path ...string) {
	if len(path) == 0 {
		return
	}
	parent := root
	for _, key := range path[:len(path)-1] {
		parent = mapValue(parent, key)
		if parent == nil {
			return
		}
	}
	if parent.Kind != yaml.MappingNode {
		return
	}
	key := path[len(path)-1]
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			// The existing node's comments belong to the key, not to the value, so replacing the
			// value keeps them.
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteKey(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// appendTo appends an element to a sequence at a path.
func appendTo(root *yaml.Node, element *yaml.Node, path ...string) {
	seq := seqAt(root, path...)
	if seq == nil {
		return
	}
	seq.Content = append(seq.Content, element)
}

// node renders a Go value as a yaml node, for splicing.
func node(value any) (*yaml.Node, error) {
	raw, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("fixture: derive: encode a manifest fragment: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fixture: derive: decode a manifest fragment: %w", err)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &doc, nil
}

// walkScalars visits every scalar in the tree, values and keys alike. Keys are visited too
// because `exonerating_evidence` is keyed by the decoy's identifier, and a permutation that
// renamed the decoy but not the key would leave a fixture that fails its own validation.
func walkScalars(n *yaml.Node, fn func(*yaml.Node)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		fn(n)
		return
	}
	for _, child := range n.Content {
		walkScalars(child, fn)
	}
}

// renderYAML writes the node tree back out at the indentation the corpus uses.
func renderYAML(root *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return nil, fmt.Errorf("fixture: derive: render the manifest: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("fixture: derive: render the manifest: %w", err)
	}
	return out.Bytes(), nil
}
