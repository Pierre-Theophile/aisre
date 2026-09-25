// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// What a revision was deployed with (T136, T137; FR-033, FR-034).

// The secret value that must never reach the graph in any form. It is a literal in a test rather than
// something resembling a credential, and every assertion below searches the emitted properties for it.
const plantedSecret = "postgres://appuser:hunter2@10.24.0.7:5432/orders?sslmode=require"

var fingerprintKey = []byte("a-test-fingerprint-key-not-a-production-one")

// revisionWithConfig renders one Cloud Run revision — the immutable snapshot of what was deployed.
//
// `deployedAt` is the revision's createTime, which is when the configuration became true.
func revisionWithConfig(name string, deployedAt time.Time, env []*runpb.EnvVar, volumes []*runpb.Volume) *runpb.Revision {
	return &runpb.Revision{
		Name:           "projects/" + sqlProject + "/locations/" + sqlRegion + "/services/checkout/revisions/" + name,
		CreateTime:     timestamppb.New(deployedAt),
		ServiceAccount: "checkout@" + sqlProject + ".iam.gserviceaccount.com",
		Containers:     []*runpb.Container{{Image: "europe-docker.pkg.dev/x/y@sha256:00", Env: env}},
		Volumes:        volumes,
	}
}

// deployedFirst and deployedSecond are the two deploy instants the config tests compare across.
var (
	deployedFirst  = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	deployedSecond = time.Date(2026, 9, 21, 2, 40, 0, 0, time.UTC)
)

// serviceWithConfig keeps the old call shape: a revision named after the generation it stands for, so
// each test still reads as "version 7 then version 8".
func serviceWithConfig(generation int64, env []*runpb.EnvVar, volumes []*runpb.Volume) *runpb.Revision {
	deployedAt := deployedFirst
	if generation > 7 {
		deployedAt = deployedSecond
	}
	return revisionWithConfig(fmt.Sprintf("checkout-000%02d-aaa", generation), deployedAt, env, volumes)
}

func literalEnv(name, value string) *runpb.EnvVar {
	return &runpb.EnvVar{Name: name, Values: &runpb.EnvVar_Value{Value: value}}
}

func secretEnv(name, secret, version string) *runpb.EnvVar {
	return &runpb.EnvVar{Name: name, Values: &runpb.EnvVar_ValueSource{
		ValueSource: &runpb.EnvVarSource{SecretKeyRef: &runpb.SecretKeySelector{
			Secret: secret, Version: version,
		}},
	}}
}

func observeConfig(t *testing.T, rev *runpb.Revision, key []byte) gcpfeeder.ConfigObservation {
	t.Helper()
	obs, err := gcpfeeder.ObserveConfig(rev, key)
	if err != nil {
		t.Fatalf("ObserveConfig: %v", err)
	}
	return obs
}

// FR-034, asserted the only way it can be: search everything the node carries for the value.
func TestNoConfigurationValueReachesTheGraph(t *testing.T) {
	obs := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", plantedSecret),
		literalEnv("LOG_LEVEL", "info"),
		secretEnv("API_TOKEN", "orders-api-token", "4"),
	}, nil), fingerprintKey)

	props := propsMap(t, obs.Props())
	for key, value := range props {
		if strings.Contains(value, plantedSecret) || strings.Contains(value, "hunter2") {
			t.Fatalf("property %s carries a configuration value: %q", key, value)
		}
	}
	// The NAMES are recorded, because a name is what an operator calls the setting and the published
	// rules read it.
	env := props[gcpfeeder.PropConfigEnvEntries]
	for _, name := range []string{"DATABASE_URL", "LOG_LEVEL", "API_TOKEN"} {
		if !strings.Contains(env, name) {
			t.Errorf("environment entry %s is not recorded at all: %q", name, env)
		}
	}
	// And a literal value is recorded as a prefixed fingerprint, which a reader cannot mistake for a
	// value.
	if !strings.Contains(env, gcpfeeder.ConfigFingerprintPrefix) {
		t.Errorf("no fingerprint is recorded for the literal entries: %q", env)
	}
}

// A secret is a version reference and never content: `<secret>@<version>`, which is what makes "the
// credential was rotated at 02:40" a fact the graph holds without holding the credential.
func TestASecretIsRecordedAsAVersionReference(t *testing.T) {
	obs := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "4"),
	}, []*runpb.Volume{{
		Name: "certs",
		VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{
			Secret: "orders-tls", Items: []*runpb.VersionToPath{{Path: "tls.key", Version: "11"}},
		}},
	}}), fingerprintKey)

	props := propsMap(t, obs.Props())
	refs := props[gcpfeeder.PropConfigSecretRefs]
	for _, want := range []string{"orders-api-token@4", "orders-tls@11"} {
		if !strings.Contains(refs, want) {
			t.Errorf("secret reference %s is missing from %q", want, refs)
		}
	}
	// A secret-sourced entry gets no fingerprint: there is no value to fingerprint, and one would be
	// a fingerprint of a reference dressed up as a fingerprint of a secret.
	for _, entry := range obs.Env {
		if entry.Name == "API_TOKEN" && entry.Fingerprint != "" {
			t.Errorf("a secret-sourced entry carries a fingerprint %q", entry.Fingerprint)
		}
	}
}

// A secret volume with no items mounts `latest`, and that is recorded as GCP's own reference rather
// than resolved to a version number — resolving it would be a claim about which version was mounted,
// made at the poll rather than at the deploy.
func TestAVersionlessSecretMountRecordsLatestRatherThanAResolvedVersion(t *testing.T) {
	obs := observeConfig(t, serviceWithConfig(7, nil, []*runpb.Volume{{
		Name:       "certs",
		VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: "orders-tls"}},
	}}), fingerprintKey)
	props := propsMap(t, obs.Props())
	if !strings.Contains(props[gcpfeeder.PropConfigSecretRefs], "orders-tls@"+gcpfeeder.SecretVersionLatest) {
		t.Errorf("secret refs = %q, want orders-tls@latest", props[gcpfeeder.PropConfigSecretRefs])
	}
}

// With no key configured, NO fingerprint is stored and the node says so. An unkeyed digest of a
// configuration value is recoverable by enumeration, so it is not a redaction.
func TestWithNoKeyNoFingerprintIsStoredAndTheOmissionIsStated(t *testing.T) {
	obs := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", plantedSecret),
	}, nil), nil)
	props := propsMap(t, obs.Props())
	if strings.Contains(props[gcpfeeder.PropConfigEnvEntries], gcpfeeder.ConfigFingerprintPrefix) {
		t.Fatalf("a fingerprint was stored with no key: %q", props[gcpfeeder.PropConfigEnvEntries])
	}
	if props[gcpfeeder.PropConfigFingerprintsUnavailable] == "" {
		t.Error("the omission is not stated, so an unchanged value and an unchecked one read alike")
	}
	if !strings.Contains(props[gcpfeeder.PropConfigEnvEntries], "DATABASE_URL") {
		t.Error("the entry's name is not recorded either; the name is not the value")
	}
	if gcpfeeder.Fingerprint(nil, "DATABASE_URL", plantedSecret) != "" {
		t.Error("Fingerprint returned a digest with no key")
	}
}

// Two entries holding one value must not fingerprint alike, or the graph quietly answers "is
// DB_PASSWORD the same string as ADMIN_PASSWORD?".
func TestTwoEntriesWithOneValueDoNotFingerprintAlike(t *testing.T) {
	first := gcpfeeder.Fingerprint(fingerprintKey, "DB_PASSWORD", "same-value")
	second := gcpfeeder.Fingerprint(fingerprintKey, "ADMIN_PASSWORD", "same-value")
	if first == second {
		t.Fatal("two entries holding one value fingerprint alike, which publishes a comparison " +
			"nobody meant to publish")
	}
	// And the same entry with the same value is stable, or no change could ever be detected.
	if first != gcpfeeder.Fingerprint(fingerprintKey, "DB_PASSWORD", "same-value") {
		t.Fatal("the fingerprint is not stable, so every poll would report a change")
	}
	// A different key gives a different fingerprint, which is what makes it keyed rather than a hash.
	if first == gcpfeeder.Fingerprint([]byte("another-key"), "DB_PASSWORD", "same-value") {
		t.Fatal("the fingerprint does not depend on the key")
	}
}

// A changed value is named and nothing more.
func TestAChangedValueIsNamedAndNotStored(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", plantedSecret),
		literalEnv("LOG_LEVEL", "info"),
	}, nil), fingerprintKey)
	after := observeConfig(t, serviceWithConfig(8, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", plantedSecret+"&application_name=orders"),
		literalEnv("LOG_LEVEL", "info"),
	}, nil), fingerprintKey)

	diff := gcpfeeder.DiffConfig(before, after)
	if len(diff.Changed) != 1 || diff.Changed[0] != "DATABASE_URL" {
		t.Fatalf("Changed = %v, want just the name", diff.Changed)
	}
	props := propsMap(t, gcpfeeder.ConfigChangeProps(before, after, diff, gcpfeeder.Actor{}))
	for key, value := range props {
		if strings.Contains(value, "hunter2") || strings.Contains(value, "application_name") {
			t.Fatalf("change property %s carries a value: %q", key, value)
		}
	}
}

// A secret moving version records both versions — they are identifiers — and earns the more specific
// taxonomy kind when it is the whole of what changed.
func TestARotatedSecretRecordsBothVersionsAndIsASecretRotation(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "4"),
	}, nil), fingerprintKey)
	after := observeConfig(t, serviceWithConfig(8, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "5"),
	}, nil), fingerprintKey)

	diff := gcpfeeder.DiffConfig(before, after)
	if len(diff.RotatedSecrets) != 1 ||
		diff.RotatedSecrets[0] != "API_TOKEN: orders-api-token@4 -> orders-api-token@5" {
		t.Fatalf("RotatedSecrets = %v, want both version references", diff.RotatedSecrets)
	}
	if !diff.OnlySecretsRotated() {
		t.Error("a rotation alone was not recognised as such")
	}
	change, err := gcpfeeder.ConfigChange(before, after, diff, gcpfeeder.Actor{})
	if err != nil {
		t.Fatalf("ConfigChange: %v", err)
	}
	at := deployedSecond
	if change.Kind != graphv1.ChangeKind_SECRET_ROTATION {
		t.Errorf("kind = %s, want SECRET_ROTATION; a reader asking when a credential was last "+
			"rotated should not have to read every config change", change.Kind)
	}
	if !change.ValidAt.Equal(at) {
		t.Errorf("valid_at = %s, want the revision's creation instant %s", change.ValidAt, at)
	}
	// The service, the configuration version and the revision are all targets, which is what gives
	// the change a `changed-by` edge from each (FR-033).
	var haveService, haveConfig, haveRevision bool
	for _, target := range change.Targets {
		switch target.GetNamespace() {
		case gcpfeeder.NSService:
			haveService = true
		case gcpfeeder.NSConfig:
			haveConfig = true
		case gcpfeeder.NSRevision:
			haveRevision = true
		}
	}
	if !haveService || !haveConfig || !haveRevision {
		t.Errorf("targets = %v, want the service, the configuration and the revision", change.Targets)
	}
}

// A rotation together with anything else is a CONFIG_CHANGE: the specific kind is for the case where
// it is the whole of what happened.
func TestARotationAlongsideOtherChangesIsAConfigChange(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "4"),
		literalEnv("LOG_LEVEL", "info"),
	}, nil), fingerprintKey)
	after := observeConfig(t, serviceWithConfig(8, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "5"),
		literalEnv("LOG_LEVEL", "debug"),
	}, nil), fingerprintKey)
	diff := gcpfeeder.DiffConfig(before, after)
	change, err := gcpfeeder.ConfigChange(before, after, diff, gcpfeeder.Actor{})
	if err != nil {
		t.Fatalf("ConfigChange: %v", err)
	}
	if change.Kind != graphv1.ChangeKind_CONFIG_CHANGE {
		t.Errorf("kind = %s, want CONFIG_CHANGE", change.Kind)
	}
}

// An entry present in both versions with no fingerprint on either side is UNDETECTABLE, not
// unchanged. Reporting "no change" when the answer is "we had no key" is the confusion FR-012 exists
// to prevent, applied to a value.
func TestAnUncheckableEntryIsUndetectableAndNotUnchanged(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", plantedSecret),
	}, nil), nil)
	after := observeConfig(t, serviceWithConfig(8, []*runpb.EnvVar{
		literalEnv("DATABASE_URL", "something-else-entirely"),
	}, nil), nil)
	diff := gcpfeeder.DiffConfig(before, after)
	if len(diff.Changed) != 0 {
		t.Errorf("Changed = %v with no key; the feeder cannot know it changed", diff.Changed)
	}
	if len(diff.Undetectable) != 1 || diff.Undetectable[0] != "DATABASE_URL" {
		t.Fatalf("Undetectable = %v, want the entry named", diff.Undetectable)
	}
	props := propsMap(t, gcpfeeder.ConfigChangeProps(before, after,
		gcpfeeder.ConfigDiff{Changed: []string{"LOG_LEVEL"}, Undetectable: diff.Undetectable},
		gcpfeeder.Actor{}))
	if props[gcpfeeder.PropConfigFingerprintsUnavailable] == "" {
		t.Error("a change reporting some entries does not say which ones could not be checked")
	}
}

// A literal becoming secret-sourced is a change whichever way it went: the KIND of the entry changed.
func TestALiteralBecomingASecretIsAChange(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("API_TOKEN", "inlined-for-now"),
	}, nil), fingerprintKey)
	after := observeConfig(t, serviceWithConfig(8, []*runpb.EnvVar{
		secretEnv("API_TOKEN", "orders-api-token", "1"),
	}, nil), fingerprintKey)
	diff := gcpfeeder.DiffConfig(before, after)
	if len(diff.Changed) != 1 || diff.Changed[0] != "API_TOKEN" {
		t.Fatalf("Changed = %v, want the entry named", diff.Changed)
	}
}

// The configuration version's identity carries the generation: a configuration IS a version, and
// without it every change would address one node and the history would be one row deep.
func TestTheConfigurationRefCarriesTheRevision(t *testing.T) {
	seven := observeConfig(t, serviceWithConfig(7, nil, nil), fingerprintKey)
	eight := observeConfig(t, serviceWithConfig(8, nil, nil), fingerprintKey)
	if seven.Ref().GetValue() == eight.Ref().GetValue() {
		t.Fatal("two revisions share a configuration ref, so every config change would address one " +
			"node and the history would be one row deep")
	}
	if !strings.HasSuffix(seven.Ref().GetValue(), "@checkout-00007-aaa") {
		t.Errorf("ref = %q, want it to carry the revision", seven.Ref().GetValue())
	}
	node := seven.NodeFact()
	if node.Type != graphv1.NodeType_CONFIG {
		t.Errorf("node type = %s, want CONFIG", node.Type)
	}
	// The valid start is the revision's createTime and never the poll: a configuration version dated
	// at the poll would move every time the connector restarted.
	if !node.ValidAt.Equal(deployedFirst) {
		t.Errorf("valid_at = %s, want the revision's createTime %s", node.ValidAt, deployedFirst)
	}
	if node.ValidFromUnknown {
		t.Error("a configuration whose revision states a createTime asserts an unknown start")
	}
}

// A revision with no createTime is an error rather than an unknown start: the field is output-only and
// documented, so its absence is a response nobody should process quietly.
func TestAConfigurationChangeNeedsTheRevisionsCreateTime(t *testing.T) {
	before := observeConfig(t, serviceWithConfig(7, []*runpb.EnvVar{
		literalEnv("LOG_LEVEL", "info"),
	}, nil), fingerprintKey)
	undated := revisionWithConfig("checkout-00008-bbb", time.Time{}, []*runpb.EnvVar{
		literalEnv("LOG_LEVEL", "debug"),
	}, nil)
	after := observeConfig(t, undated, fingerprintKey)
	if !after.CreateTime.IsZero() {
		t.Fatalf("createTime = %s, want zero for a revision that states none", after.CreateTime)
	}
	if _, err := gcpfeeder.ConfigChange(before, after, gcpfeeder.DiffConfig(before, after),
		gcpfeeder.Actor{}); err == nil {
		t.Fatal("a configuration change with no instant was accepted")
	}
}
