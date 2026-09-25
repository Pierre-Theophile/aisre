// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// Environment variables as CONFIG changes (004 T095–T101; FR-038, FR-039, SC-006).

// theSecret is the value these tests plant and then look for. It never reaches any mapper — it exists so
// that the "no value in any form" assertion has something to fail on when it is probed (T098).
const theSecret = "postgres://payments:hunter2@db.internal:5432/payments"

var configAt = time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)

// createdAt and updatedAt are the platform's own instants, hours before the observation, because the
// whole of FR-039 is that a configuration change is dated by when the platform says it changed.
var (
	configCreated = time.Date(2026, 9, 21, 3, 10, 0, 0, time.UTC)
	configEdited  = time.Date(2026, 9, 21, 9, 5, 0, 0, time.UTC)
)

func productionVariable() vercelfeeder.ProjectEnv {
	return vercelfeeder.ProjectEnv{
		ID:        "icfg_payments_url",
		Key:       "PAYMENTS_API_URL",
		Target:    []string{"production"},
		Type:      "encrypted",
		CreatedBy: "usr_ada",
		CreatedAt: configCreated.UnixMilli(),
	}
}

func mapVariable(t *testing.T, env vercelfeeder.ProjectEnv) vercelfeeder.ConfigChange {
	t.Helper()
	got, err := mapper(t).MapProjectEnv(mapProject, env, configAt)
	if err != nil {
		t.Fatalf("MapProjectEnv: %v", err)
	}
	return got
}

func TestAProductionVariableBecomesAConfigChange(t *testing.T) {
	t.Parallel()
	got := mapVariable(t, productionVariable())
	if got.Excluded != "" {
		t.Fatalf("excluded as %q; a production variable is a configuration change", got.Excluded)
	}
	change := got.Change.GetObserveChange()
	if change == nil {
		t.Fatal("no change event")
	}
	if change.GetChange().GetKind() != graphv1.ChangeKind_CONFIG_CHANGE {
		t.Errorf("kind = %v, want CONFIG_CHANGE", change.GetChange().GetKind())
	}
	// The key, which is the whole investigative payload of a configuration change.
	props := propsOf(t, got.Change)
	if props["sre.vercel.config_key"] != "PAYMENTS_API_URL" {
		t.Errorf("config_key = %q, want the variable's key", props["sre.vercel.config_key"])
	}
	if props["sre.vercel.config_operation"] != "created" {
		t.Errorf("operation = %q, want created: updatedAt is not later than createdAt",
			props["sre.vercel.config_operation"])
	}
	if props["sre.vercel.env_id"] != "icfg_payments_url" {
		t.Errorf("env_id = %q, want the variable's id", props["sre.vercel.env_id"])
	}
	// The project itself, which this feeder describes, and then the target the operator maps it to — so
	// the change attaches to a node that exists as well as to the service elsewhere (004 T093).
	if targets := change.GetTargets(); len(targets) != 2 ||
		targets[0].GetNamespace() != "vercel.project" || targets[0].GetValue() != "prj_storefront" ||
		targets[1].GetValue() != "shop/storefront" {
		t.Errorf("targets = %v, want the project and then the service it maps to", targets)
	}
}

// FR-039's first clause: the instant is the platform's, not the poll's.
func TestTheChangeIsDatedWhenThePlatformSaysTheVariableChanged(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.UpdatedBy = "usr_grace"
	env.UpdatedAt = configEdited.UnixMilli()

	got := mapVariable(t, env)
	change := got.Change.GetObserveChange()
	if !change.GetValidAt().AsTime().Equal(configEdited) {
		t.Errorf("ValidAt = %v, want the platform's updatedAt (%v). The poll instant is %v and using it "+
			"would date an 09:05 edit at 14:30",
			change.GetValidAt().AsTime(), configEdited, configAt)
	}
	props := propsOf(t, got.Change)
	if props["sre.vercel.config_operation"] != "updated" {
		t.Errorf("operation = %q, want updated: updatedAt is later than createdAt",
			props["sre.vercel.config_operation"])
	}
	// The creation instant survives the update, so nothing is lost by dating the change from the edit.
	if props["sre.vercel.config_created_at"] != configCreated.Format(time.RFC3339) {
		t.Errorf("config_created_at = %q, want the creation instant kept as a property",
			props["sre.vercel.config_created_at"])
	}
}

// T097 and SC-006, the assertion this whole user story turns on: no value in any form, anywhere.
//
// The check runs over the WHOLE serialised event rather than over a list of properties, because a list
// of properties is a list somebody has to remember to extend. Serialising and searching the bytes covers
// the summary, the actor, every property, every claim and every pointer at once — including a field added
// next year by somebody who never read this test.
func TestNoValueAppearsInAnyFormAnywhereInTheEvent(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.UpdatedBy = "usr_grace"
	env.UpdatedAt = configEdited.UnixMilli()

	got := mapVariable(t, env)
	body, err := json.Marshal(got.Change)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	haystack := string(body)

	// Every form FR-038 names, and the two ciphertext carriers the platform's own response schema has
	// beside the plaintext.
	digest := sha256.Sum256([]byte(theSecret))
	forms := map[string]string{
		"the plaintext value":      theSecret,
		"the value's host":         "db.internal",
		"the value's password":     "hunter2",
		"a truncation of it":       theSecret[:12],
		"a sha256 hash of it":      hex.EncodeToString(digest[:]),
		"a truncated hash of it":   hex.EncodeToString(digest[:])[:16],
		"the legacy ciphertext":    "legacyValue",
		"the encrypted-value hint": "encryptedValue",
	}
	for what, form := range forms {
		if strings.Contains(haystack, form) {
			t.Errorf("the event carries %s (%q). FR-038 forbids the value plaintext, ciphertext, "+
				"truncated or hashed, and SC-006 counts hashes among what must be zero", what, form)
		}
	}
}

// The structural half of T097, and the one a probe cannot get past by luck.
//
// The reason the assertion above can be trusted is not that it searches hard — it is that this process
// never has the value to leak. `ProjectEnv` has no field for `value`, `legacyValue` or
// `internalContentHint`, so a response carrying all three decodes into a struct that keeps none of them.
// This test reads the real response shape from the published schema, decodes it, and re-serialises what
// the struct kept.
func TestThePayloadsValueFieldsHaveNowhereToLand(t *testing.T) {
	t.Parallel()
	// The response as the platform documents it, with all three carriers populated.
	raw := `{"envs":[{
		"id":"icfg_payments_url","key":"PAYMENTS_API_URL","target":["production"],
		"type":"encrypted","createdBy":"usr_ada","createdAt":1758424200000,
		"value":"` + theSecret + `",
		"legacyValue":"AQICAHh7ZmFrZWNpcGhlcnRleHQ=",
		"internalContentHint":{"type":"flags-secret","encryptedValue":"AQICAHh7YW5vdGhlcg=="},
		"decrypted":false,"securityIssues":[]
	}]}`
	var body struct {
		Envs []vercelfeeder.ProjectEnv `json:"envs"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Envs) != 1 {
		t.Fatalf("decoded %d variables, want 1", len(body.Envs))
	}
	// What survived the decode. If the struct ever gains a field for any carrier, it appears here.
	kept, err := json.Marshal(body.Envs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, carrier := range []string{theSecret, "AQICAHh7ZmFrZWNpcGhlcnRleHQ=", "AQICAHh7YW5vdGhlcg=="} {
		if strings.Contains(string(kept), carrier) {
			t.Errorf("ProjectEnv kept %q from the response. The platform sends the secret on every "+
				"read and the only thing keeping it out of this process is that the struct has nowhere "+
				"to put it (FR-038)", carrier)
		}
	}
	// And the key did survive, so the test above is not passing because nothing decoded at all.
	if body.Envs[0].Key != "PAYMENTS_API_URL" {
		t.Errorf("key = %q, want the decode to have worked", body.Envs[0].Key)
	}
}

// T101: the environment filter, and the count.
func TestAPreviewOnlyVariableIsExcludedAndCounted(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.Target = []string{"preview", "development"}

	got := mapVariable(t, env)
	if got.Change != nil {
		t.Error("a preview-only variable produced a change; production is what this corpus is about")
	}
	if got.Excluded != vercelfeeder.ExcludedPreviewOnlyVariable {
		t.Errorf("excluded = %q, want %q", got.Excluded, vercelfeeder.ExcludedPreviewOnlyVariable)
	}
	// Counted, because FR-032 and SC-002 ask for the refusals to be reported rather than logged.
	counts := vercelfeeder.CountConfigExclusions([]vercelfeeder.ConfigChange{got, got,
		{Excluded: vercelfeeder.ExcludedSystemVariable}})
	want := map[string]int{
		vercelfeeder.ExcludedPreviewOnlyVariable: 2,
		vercelfeeder.ExcludedSystemVariable:      1,
	}
	if len(counts) != len(want) {
		t.Fatalf("counted %d reasons, want %d: %v", len(counts), len(want), counts)
	}
	for _, c := range counts {
		if want[c.Reason] != c.Count {
			t.Errorf("%s = %d, want %d", c.Reason, c.Count, want[c.Reason])
		}
	}
}

// A variable scoped to production AND preview is still a production variable. The filter asks whether
// production is among the environments, not whether it is the only one.
func TestAVariableScopedToProductionAndPreviewIsAChange(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.Target = []string{"preview", "production", "development"}

	got := mapVariable(t, env)
	if got.Excluded != "" {
		t.Fatalf("excluded as %q; production is among its environments", got.Excluded)
	}
	props := propsOf(t, got.Change)
	// Sorted and de-duplicated, so the golden does not move when the platform reorders the array.
	if props["sre.vercel.config_environments"] != "development, preview, production" {
		t.Errorf("config_environments = %q, want all three sorted",
			props["sre.vercel.config_environments"])
	}
}

func TestASystemVariableIsExcludedAndCounted(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.Key = "VERCEL_URL"
	env.System = true

	got := mapVariable(t, env)
	if got.Change != nil {
		t.Error("a platform-assigned variable produced a change nobody performed")
	}
	if got.Excluded != vercelfeeder.ExcludedSystemVariable {
		t.Errorf("excluded = %q, want %q", got.Excluded, vercelfeeder.ExcludedSystemVariable)
	}
}

// FR-012, applied to configuration: a variable with no stated instant is refused rather than dated from
// the poll. The poll instant is right there in the call, which is exactly why this needs a test.
func TestAVariableWithNoStatedInstantIsExcludedRatherThanDatedFromThePoll(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.CreatedAt = 0
	env.UpdatedAt = 0

	got := mapVariable(t, env)
	if got.Change != nil {
		t.Error("a variable with no stated instant produced a change; its valid time could only have " +
			"come from the poll, which FR-012 forbids guessing")
	}
	if got.Excluded != vercelfeeder.ExcludedUnusableVariable {
		t.Errorf("excluded = %q, want %q", got.Excluded, vercelfeeder.ExcludedUnusableVariable)
	}
}

// The actor is named and NOT typed, which is the honest answer rather than the convenient one.
func TestTheActorIsNamedAndTheKindIsUnknownRatherThanGuessed(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.UpdatedBy = "usr_grace"
	env.UpdatedAt = configEdited.UnixMilli()

	got := mapVariable(t, env)
	change := got.Change.GetObserveChange()
	// The editor, not the creator: the change being described is the edit.
	if change.GetChange().GetActor() != "usr_grace" {
		t.Errorf("actor = %q, want the id Vercel credits with the edit",
			change.GetChange().GetActor())
	}
	if change.GetChange().GetActorKind() != graphv1.ActorKind_ACTOR_KIND_UNKNOWN {
		t.Errorf("actor kind = %v, want UNKNOWN. Nothing in the response types the actor, and FR-013 "+
			"forbids typing one from its string — a variable edited by hand is the USUAL case, not a "+
			"stated one", change.GetChange().GetActorKind())
	}
	// And the evidence says why, so a reader can see the kind was derived from fields.
	props := propsOf(t, got.Change)
	evidence := props["sre.vercel.config_actor_evidence"]
	for _, want := range []string{"actor_type=absent", "updated_by=present", "created_by=present"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("actor evidence %q does not state %q", evidence, want)
		}
	}
	// The evidence records PRESENCE and never the id, because an id that identifies a person is a
	// people identifier and the policy drops those rather than repeating them in a second field.
	if strings.Contains(evidence, "usr_grace") || strings.Contains(evidence, "usr_ada") {
		t.Errorf("actor evidence %q repeats the actor id; presence is what the evidence is for", evidence)
	}
}

// With no actor stated at all the kind is UNSPECIFIED, which is a different claim from UNKNOWN.
func TestNoActorStatedIsUnspecifiedRatherThanUnknown(t *testing.T) {
	t.Parallel()
	env := productionVariable()
	env.CreatedBy = ""
	env.UpdatedBy = ""

	got := mapVariable(t, env)
	if kind := got.Change.GetObserveChange().GetChange().GetActorKind(); kind !=
		graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		t.Errorf("actor kind = %v, want UNSPECIFIED: the source said nothing, which is not the same as "+
			"naming an actor we could not type", kind)
	}
}

// Two edits of one variable are two changes, and one edit read twice is one change.
//
// The ref carries the operation instant, which is the platform's statement about the CHANGE. That is why
// it is safe here and would not be for a poll instant: reading the same edit twice produces the same ref
// and the log answers DUPLICATE_NOOP, while a genuinely later edit produces a different one.
func TestTheRefDistinguishesTwoEditsAndNotTwoReadings(t *testing.T) {
	t.Parallel()
	first := productionVariable()
	first.UpdatedBy = "usr_grace"
	first.UpdatedAt = configEdited.UnixMilli()

	second := first
	second.UpdatedAt = configEdited.Add(time.Hour).UnixMilli()

	// The same edit, read at two different times.
	readEarly := mapVariable(t, first)
	readLate, err := mapper(t).MapProjectEnv(mapProject, first, configAt.Add(6*time.Hour))
	if err != nil {
		t.Fatalf("MapProjectEnv: %v", err)
	}
	if readEarly.Change.GetEventId() != readLate.Change.GetEventId() {
		t.Errorf("one edit read twice produced two event ids (%q and %q); an extra poll must not write "+
			"a new fact", readEarly.Change.GetEventId(), readLate.Change.GetEventId())
	}

	// A genuinely later edit.
	later := mapVariable(t, second)
	if readEarly.Change.GetEventId() == later.Change.GetEventId() {
		t.Errorf("two edits an hour apart share the event id %q, so the second would be swallowed as a "+
			"duplicate", later.Change.GetEventId())
	}
}

// propsOf reads a change's properties into a flat map, joining repeated values so a test can assert on
// one string.
func propsOf(t *testing.T, event *graphv1.EventEnvelope) map[string]string {
	t.Helper()
	out := map[string]string{}
	for key, value := range event.GetObserveChange().GetProps().GetFields() {
		if list := value.GetListValue(); list != nil {
			parts := make([]string, 0, len(list.GetValues()))
			for _, item := range list.GetValues() {
				parts = append(parts, item.GetStringValue())
			}
			out[key] = strings.Join(parts, ", ")
			continue
		}
		out[key] = value.GetStringValue()
	}
	return out
}

// The sanitisation disposition for the two fields this change adds, asserted here rather than left to
// T106's table. A payload field naming a person that the vocabulary does not know is dropped by nothing,
// and the day that is discovered is the day a corpus is published.
func TestTheActorFieldsAreKnownToTheSanitisationVocabulary(t *testing.T) {
	t.Parallel()
	for _, field := range []string{
		"createdBy", "created_by", "updatedBy", "updated_by",
		"envs[].createdBy", "envs[].updatedBy",
	} {
		if !sanitise.IsPeopleField(field) {
			t.Errorf("%q is not a people field under the published vocabulary, so a recording would "+
				"carry the id of whoever edited a variable (FR-061)", field)
		}
	}
	// Not vacuous: a field that names no person is not a people field.
	for _, field := range []string{"key", "envs[].key", "updatedAt"} {
		if sanitise.IsPeopleField(field) {
			t.Errorf("%q was treated as a people field; the vocabulary has grown too generous to mean "+
				"anything", field)
		}
	}
}
