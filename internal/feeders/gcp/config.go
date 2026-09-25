// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What a revision was deployed with (T136, T137; FR-033, FR-034).
//
// # The rule, and why it is a rule about what is NOT stored
//
// FR-033 asks for what a revision was deployed with: environment variable **names** and value
// **fingerprints**, mounted configuration references, and secret **version references** — with a
// configuration change when any of them changes between revisions. FR-034 forbids storing secret
// material or the value of any entry the published rules classify as sensitive.
//
// Those two read as one requirement and one caveat. They are better read as one design: a fingerprint
// answers *"did this change?"* without storing what it is, and that is the only question an
// investigation asks of a configuration value. "The database URL changed at 03:12" is actionable. The
// URL itself is not more actionable — it is the same fact plus a credential in a graph that is queried
// by a chat bot.
//
// # A fingerprint needs a key, or it is a value anybody can recover
//
// An unkeyed digest of a configuration value is not a redaction. Configuration values come from a
// small, guessable space — `true`, `false`, `8080`, `production`, a region name, an image tag — so
// sha256("true") is a lookup away from "true" for anybody holding the graph. It is worse for the
// values that matter: a digest of a password is a password hash, and an unsalted one.
//
// So a fingerprint is a **keyed HMAC**, the key is operator configuration, and where no key is
// configured **no fingerprint is stored at all**. The entry's name is still recorded, and the
// checkpoint states that value-change detection was unavailable and why. An absent fingerprint reads
// as "we did not say", which is true; a crackable one reads as a redaction, which is false. This is
// the same rule internal/investigation/backend's redactor states for pseudonyms, applied to the same
// hazard on the feeder side.
//
// The key is mixed with the entry's **name** as well as its value, so that two entries holding one
// value do not fingerprint alike. Otherwise the graph would quietly answer "is DB_PASSWORD the same
// string as ADMIN_PASSWORD?", which is a question nobody meant to publish.
//
// # A secret is a version reference and never a value
//
// A secret-sourced environment variable and a mounted secret volume are recorded as
// `<secret>@<version>` — the reference, which is an identifier — and never as content. The feeder has
// no permission to read Secret Manager and would not record the result if it had. The version is the
// load-bearing half: FR-034's "record only that the entry changed, and the version reference it moved
// to" is exactly what makes "the credential was rotated at 02:40" a fact the graph holds, and it
// holds it without holding the credential.

// ConfigFingerprintPrefix marks a value as a keyed fingerprint rather than a configuration value, so
// that nobody reading a golden mistakes one for the other.
const ConfigFingerprintPrefix = "fp_"

// ConfigFingerprintLength is how many hex characters of the HMAC are kept. 32 hex characters — 128
// bits — is far past any collision concern for the number of entries one service has, and the reason
// it is truncated at all is that the whole digest is noise in a diff a human reads.
const ConfigFingerprintLength = 32

// The configuration properties.
const (
	// PropConfigOf is the service this configuration version belongs to.
	PropConfigOf = "sre.gcp.config_of"
	// PropConfigRevision is the revision this configuration is the snapshot of, which is the version
	// half of the config node's identity.
	PropConfigRevision = "sre.gcp.config_revision"
	// PropConfigEnvEntries is the environment, as `name=<fingerprint-or-reference>` entries. Never a
	// value: see the file comment.
	PropConfigEnvEntries = "sre.gcp.config_env"
	// PropConfigSecretRefs is the secret version references, as `<secret>@<version>` — mounted and
	// environment-sourced alike.
	PropConfigSecretRefs = "sre.gcp.config_secret_refs"
	// PropConfigMounts is the mounted configuration references, as `<mount-path>=<source>`.
	PropConfigMounts = "sre.gcp.config_mounts"
	// PropConfigCloudSQLInstances is the Cloud SQL instances attached to the revision, by connection
	// name. This is a *configured attachment* and it is what FR-028's derived edge rests on.
	PropConfigCloudSQLInstances = "sre.gcp.config_cloudsql_instances"
	// PropConfigServiceAccount is the service account the revision runs as. It is an identity, not a
	// credential, and it is what an IAM change is about.
	PropConfigServiceAccount = "sre.gcp.config_service_account"
	// PropConfigFingerprintsUnavailable states that no fingerprint key was configured, so value
	// changes cannot be detected. It is a positive statement of an omission (FR-012).
	PropConfigFingerprintsUnavailable = "sre.gcp.config_fingerprints_unavailable"
	// PropConfigChangedEntries is on the change: which entries changed, by name and nothing else.
	PropConfigChangedEntries = "sre.gcp.config_changed_entries"
	// PropConfigAddedEntries and PropConfigRemovedEntries are the other two halves of the diff.
	PropConfigAddedEntries   = "sre.gcp.config_added_entries"
	PropConfigRemovedEntries = "sre.gcp.config_removed_entries"
	// PropConfigFrom is the revision this configuration changed FROM, so a reader can see which two
	// versions were compared rather than having to find the predecessor themselves.
	PropConfigFrom = "sre.gcp.config_changed_from"
	// PropConfigRotatedSecrets is which secret references moved, as `<secret>: <old> -> <new>`. The
	// versions are identifiers, so they are recorded in full.
	PropConfigRotatedSecrets = "sre.gcp.config_rotated_secrets"
)

// FingerprintsUnavailable is what a configuration node says when no key was configured.
const FingerprintsUnavailable = "no configuration fingerprint key is configured, so a change to an " +
	"environment variable's value cannot be detected. An unkeyed digest of a configuration value is " +
	"recoverable by enumeration, so none is stored (FR-034)"

// ConfigEntry is one environment variable as this feeder is willing to record it.
type ConfigEntry struct {
	// Name is the variable's name, recorded verbatim. A name is not configuration content: it is
	// what an operator calls the setting, and the published rules read it (P4, FR-121).
	Name string
	// Fingerprint is the keyed fingerprint of a literal value, prefixed. Empty for a secret-sourced
	// variable — there is no value to fingerprint — and empty when no key is configured.
	Fingerprint string
	// SecretRef is `<secret>@<version>` for a variable sourced from Secret Manager.
	SecretRef string
}

// Value renders the entry as the graph stores it: the name, and the fingerprint or the reference.
func (e ConfigEntry) Value() string {
	switch {
	case e.SecretRef != "":
		return e.Name + "=" + e.SecretRef
	case e.Fingerprint != "":
		return e.Name + "=" + e.Fingerprint
	default:
		// The name alone. It says the variable is set and says nothing about its value, which is
		// exactly what is known when there is no key.
		return e.Name
	}
}

// ConfigObservation is one version of what a Cloud Run service was deployed with.
type ConfigObservation struct {
	// Revision is the revision this configuration is the snapshot of. A Cloud Run revision is
	// immutable, so it **is** the version: the containers, the environment, the volumes and the
	// service account, frozen at deploy time.
	Revision Revision
	// CreateTime is the revision's creation instant, which is when this configuration became true.
	// It is output-only and documented, so a missing one is an error rather than an unknown start.
	CreateTime time.Time
	// Env is the environment, sorted by name.
	Env []ConfigEntry
	// SecretRefs is every secret version reference in the revision, environment-sourced and mounted
	// alike, sorted and de-duplicated.
	SecretRefs []string
	// Mounts is the mounted configuration references, as `<name>=<source>`.
	Mounts []string
	// CloudSQLInstances is the Cloud SQL instances attached to the revision, by connection name.
	CloudSQLInstances []string
	// ServiceAccount is the identity the revision runs as.
	ServiceAccount string
	// EnvValuesNamingInstances maps an environment variable's name to the Cloud SQL connection name
	// its **value** contains, for the variables whose value the feeder could read.
	//
	// It is derivation input and is **never** emitted. A connection name is not sensitive — it is
	// published in the console and claimed as an identifier — but the map exists so depends.go can
	// assert an edge from a configured fact, and putting it on the node would be storing a value the
	// fingerprint rule says is not stored. It is the difference between reading a value and
	// recording it.
	EnvValuesNamingInstances map[string]string
	// Fingerprinted says whether a key was configured. False means Env carries names without
	// fingerprints and value changes are undetectable, which the node states.
	Fingerprinted bool
}

// ObserveConfig reads one service's configuration version (T136, T137).
//
// `key` is the fingerprint key. An empty key produces entries with names and no fingerprints, and the
// observation records that — rather than producing digests anybody can reverse.
func ObserveConfig(rev *runpb.Revision, key []byte) (ConfigObservation, error) {
	coords, ok := ParseRevisionResourceName(rev.GetName())
	if !ok {
		return ConfigObservation{}, fmt.Errorf("gcp: %q is not a Cloud Run revision resource name, so "+
			"the configuration it snapshots has no ref to be addressed by", rev.GetName())
	}
	obs := ConfigObservation{
		Revision:                 coords,
		CreateTime:               instantOf(rev.GetCreateTime()),
		ServiceAccount:           rev.GetServiceAccount(),
		EnvValuesNamingInstances: map[string]string{},
		Fingerprinted:            len(key) > 0,
	}

	secrets := map[string]bool{}
	for _, container := range rev.GetContainers() {
		for _, env := range container.GetEnv() {
			name := strings.TrimSpace(env.GetName())
			if name == "" {
				continue
			}
			entry := ConfigEntry{Name: name}
			if ref := env.GetValueSource().GetSecretKeyRef(); ref != nil {
				entry.SecretRef = secretReference(ref.GetSecret(), ref.GetVersion())
				secrets[entry.SecretRef] = true
			} else if value := env.GetValue(); value != "" {
				if obs.Fingerprinted {
					entry.Fingerprint = Fingerprint(key, name, value)
				}
				// Read for derivation, recorded nowhere. See EnvValuesNamingInstances.
				if connection, found := connectionNameIn(value); found {
					obs.EnvValuesNamingInstances[name] = connection
				}
			}
			obs.Env = append(obs.Env, entry)
		}
	}
	sort.Slice(obs.Env, func(i, j int) bool { return obs.Env[i].Name < obs.Env[j].Name })

	for _, volume := range rev.GetVolumes() {
		switch {
		case volume.GetCloudSqlInstance() != nil:
			obs.CloudSQLInstances = append(obs.CloudSQLInstances, volume.GetCloudSqlInstance().GetInstances()...)
			obs.Mounts = append(obs.Mounts, volume.GetName()+"=cloudsql")
		case volume.GetSecret() != nil:
			source := volume.GetSecret()
			obs.Mounts = append(obs.Mounts, volume.GetName()+"=secret:"+source.GetSecret())
			if len(source.GetItems()) == 0 {
				// A secret volume with no items mounts the secret's latest version, and GCP states
				// no version here. `latest` is recorded as the reference GCP itself uses — it is
				// what the deployment says, not a guess about which version that resolves to.
				secrets[secretReference(source.GetSecret(), "")] = true
				continue
			}
			for _, item := range source.GetItems() {
				secrets[secretReference(source.GetSecret(), item.GetVersion())] = true
			}
		default:
			obs.Mounts = append(obs.Mounts, volume.GetName()+"=other")
		}
	}
	obs.SecretRefs = sortedUniqueNames(keysOf(secrets))
	obs.CloudSQLInstances = sortedUniqueNames(obs.CloudSQLInstances)
	sort.Strings(obs.Mounts)
	return obs, nil
}

// SecretVersionLatest is the reference GCP uses for an unspecified secret version. It is recorded as
// itself rather than resolved: resolving it would be a claim about which version was mounted, and
// that claim would be made at the poll rather than at the deploy.
const SecretVersionLatest = "latest"

// secretReference renders `<secret>@<version>`.
func secretReference(secret, version string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	if strings.TrimSpace(version) == "" {
		version = SecretVersionLatest
	}
	return secret + "@" + version
}

// Fingerprint returns the keyed fingerprint of one configuration entry.
//
// The name is mixed in with the value so that two entries holding one string do not fingerprint
// alike — see the file comment. The key is required: callers check `len(key) > 0` first, and this
// returns the empty string rather than an unkeyed digest if they did not.
func Fingerprint(key []byte, name, value string) string {
	if len(key) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	// Length-prefixed, so that ("AB","C") and ("A","BC") do not fingerprint alike.
	fmt.Fprintf(mac, "%d:%s|%d:%s", len(name), name, len(value), value)
	return ConfigFingerprintPrefix + hex.EncodeToString(mac.Sum(nil))[:ConfigFingerprintLength]
}

// connectionNameIn finds a Cloud SQL instance connection name inside a configuration value.
//
// It is a **whole-token** match against a **strict** shape, and both halves are load-bearing. This
// function reads free text — a DSN, a comma-separated list, whatever an operator typed — and what it
// finds asserts an edge, so its false positives become dependencies on instances that do not exist.
//
// The value is split on every character that separates a connection name from what surrounds it,
// which is everything except the colons inside it. Then each token has to look like a connection name
// rather than merely have three colon-separated parts:
//
//	redis://cache.internal:6379/0  → tokens `redis:`, `cache.internal:6379`, `0` — none matches
//	postgres://u:p@/db?host=/cloudsql/p:europe-west1:orders → the last token matches
//	a:b:c                          → `b` is not shaped like a region
//
// `parseConnectionName` stays deliberately lenient by comparison, because its callers are reading a
// field GCP **states** is a connection name — a `cloud_sql_instance` volume, an instance's own
// `connectionName` — where the string is authoritative and a stricter check would reject a real
// instance. Reading a field and searching free text are different jobs with different error costs.
//
// Only the first match is returned. A variable naming two instances is a variable whose dependency is
// not derivable from it, and the proposal path is where that belongs.
func connectionNameIn(value string) (string, bool) {
	for _, token := range strings.FieldsFunc(value, isConnectionNameSeparator) {
		if !looksLikeConnectionName(token) {
			continue
		}
		if _, ok := parseConnectionName(token); ok {
			return token, true
		}
	}
	return "", false
}

// isConnectionNameSeparator reports whether a rune separates a connection name from its surroundings.
// The colon is absent on purpose: it is what a connection name is made of.
func isConnectionNameSeparator(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ',', ';', '=', '?', '&', '|', '"', '\'', '`',
		'(', ')', '[', ']', '{', '}', '<', '>', '/', '@', '\\':
		return true
	default:
		return false
	}
}

// connectionNameRegionPattern is the shape of a GCP region: `europe-west1`, `us-central1`,
// `me-central2`, `northamerica-south1`. A Cloud SQL instance's connection name carries a **region**
// and never a zone, so this is a strong discriminator against a token that merely has three parts.
var connectionNameRegionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)

// connectionNamePartPattern is the shape of a project id and an instance name: lowercase letters,
// digits and hyphens. The absence of the dot is what rules out a host name.
//
// A legacy **domain-scoped** project id (`example.com:my-project`) does not match, and cannot: it
// contains a colon, so its connection name has four parts and no three-part parse describes it. Such a
// name is left unmatched rather than mis-parsed into a ref for a project that does not exist, and the
// dependency then reaches the proposal path instead of being asserted wrongly.
var connectionNamePartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// looksLikeConnectionName reports whether a token has the shape of `<project>:<region>:<instance>`.
func looksLikeConnectionName(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 3 {
		return false
	}
	return connectionNamePartPattern.MatchString(parts[0]) &&
		connectionNameRegionPattern.MatchString(parts[1]) &&
		connectionNamePartPattern.MatchString(parts[2])
}

// keysOf returns a map's keys.
func keysOf(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	return out
}

// NodeFact renders the configuration version as the CONFIG node it is.
//
// Its valid start is the revision's `createTime`, which is when the configuration became true. That
// field is output-only and documented, so a zero one is an error at the caller rather than an unknown
// start here: a configuration version dated at the poll would move every time the connector restarted.
func (o ConfigObservation) NodeFact() feeder.NodeFact {
	return feeder.NodeFact{
		Ref:         o.Ref(),
		Type:        graphv1.NodeType_CONFIG,
		DisplayName: o.Revision.Revision + " configuration",
		ValidAt:     o.CreateTime,
	}
}

// Ref is the configuration version's addressing ref.
func (o ConfigObservation) Ref() *graphv1.Ref { return o.Revision.ConfigRef() }

// Props renders the configuration's properties.
func (o ConfigObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Revision.Project).
		Str(PropRegion, o.Revision.Region).
		Str(PropConfigOf, o.Revision.Service.Value()).
		Str(PropConfigRevision, o.Revision.Revision)
	if entries := o.EnvEntries(); len(entries) > 0 {
		props = props.Strs(PropConfigEnvEntries, entries...)
	}
	if len(o.SecretRefs) > 0 {
		props = props.Strs(PropConfigSecretRefs, o.SecretRefs...)
	}
	if len(o.Mounts) > 0 {
		props = props.Strs(PropConfigMounts, o.Mounts...)
	}
	if len(o.CloudSQLInstances) > 0 {
		props = props.Strs(PropConfigCloudSQLInstances, o.CloudSQLInstances...)
	}
	if o.ServiceAccount != "" {
		props = props.Str(PropConfigServiceAccount, o.ServiceAccount)
	}
	if !o.Fingerprinted {
		props = props.Str(PropConfigFingerprintsUnavailable, FingerprintsUnavailable)
	}
	return props
}

// EnvEntries renders the environment as the node stores it.
func (o ConfigObservation) EnvEntries() []string {
	out := make([]string, 0, len(o.Env))
	for _, entry := range o.Env {
		out = append(out, entry.Value())
	}
	return out
}

// Claims returns the configuration version's identifiers, including the addressing ref (FR-115).
func (o ConfigObservation) Claims() []Claim {
	return []Claim{{
		Namespace: NSConfig, Value: o.Ref().GetValue(),
		Why: "the ref this feeder addresses the configuration version by",
	}}
}

// ConfigDiff is what changed between two configuration versions.
type ConfigDiff struct {
	// Added, Removed and Changed are entry **names**. A changed entry is named and nothing more:
	// "DATABASE_URL changed" is the fact, and the two values are not stored (FR-034).
	Added   []string
	Removed []string
	Changed []string
	// RotatedSecrets is `<secret>: <old> -> <new>` for a secret reference that moved version. The
	// versions are identifiers, so both are recorded — this is the "version reference it moved to"
	// FR-034 requires in as many words.
	RotatedSecrets []string
	// Undetectable names the entries whose change could not be detected because no fingerprint key
	// was configured. It is not "these did not change": it is "we could not tell", and a diff that
	// reported the first would be claiming a poll's ignorance as a fact.
	Undetectable []string
}

// Empty reports whether nothing detectable changed.
func (d ConfigDiff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0 && len(d.RotatedSecrets) == 0
}

// OnlySecretsRotated reports whether the only detected change is a secret moving version, which is
// the case that earns the more specific taxonomy kind.
func (d ConfigDiff) OnlySecretsRotated() bool {
	return len(d.RotatedSecrets) > 0 && len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Summary renders the diff as one line.
func (d ConfigDiff) Summary() string {
	var parts []string
	if n := len(d.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d entry(ies) added", n))
	}
	if n := len(d.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d entry(ies) removed", n))
	}
	if n := len(d.Changed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d value(s) changed", n))
	}
	if n := len(d.RotatedSecrets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d secret(s) rotated", n))
	}
	return strings.Join(parts, ", ")
}

// DiffConfig compares two configuration versions (T136).
//
// An entry present in both whose fingerprints are both empty is **undetectable**, not unchanged. The
// distinction is the whole of FR-012 applied to a value: reporting "no change" when the answer is
// "we had no key" would make a missing configuration change indistinguishable from an absent one.
func DiffConfig(before, after ConfigObservation) ConfigDiff {
	var diff ConfigDiff
	previous := make(map[string]ConfigEntry, len(before.Env))
	for _, entry := range before.Env {
		previous[entry.Name] = entry
	}
	for _, entry := range after.Env {
		old, had := previous[entry.Name]
		if !had {
			diff.Added = append(diff.Added, entry.Name)
			continue
		}
		switch {
		case old.SecretRef != "" && entry.SecretRef != "" && old.SecretRef != entry.SecretRef:
			// Both sides are secret references, so the change is a rotation and the versions are
			// identifiers: both are recorded.
			diff.RotatedSecrets = append(diff.RotatedSecrets,
				entry.Name+": "+old.SecretRef+" -> "+entry.SecretRef)
		case (old.SecretRef == "") != (entry.SecretRef == ""):
			// A literal became a secret or the other way round. The *kind* of the entry changed,
			// which is a change whichever way it went.
			diff.Changed = append(diff.Changed, entry.Name)
		case old.Fingerprint == "" && entry.Fingerprint == "" && entry.SecretRef == "":
			diff.Undetectable = append(diff.Undetectable, entry.Name)
		case old.Fingerprint != entry.Fingerprint:
			diff.Changed = append(diff.Changed, entry.Name)
		}
	}
	for _, entry := range before.Env {
		if !containsEntry(after.Env, entry.Name) {
			diff.Removed = append(diff.Removed, entry.Name)
		}
	}
	// Mounted secrets, which are not environment entries and rotate independently.
	//
	// A secret already reported through an environment entry is skipped. Every secret reference —
	// mounted or environment-sourced — is in SecretRefs, so without this filter one rotation of one
	// secret-sourced variable would be reported twice: once named by its variable and once by the
	// secret, which reads as two rotations of two things.
	diff.RotatedSecrets = append(diff.RotatedSecrets,
		rotatedMounts(before.SecretRefs, after.SecretRefs, secretsNamedByEnv(after.Env))...)
	sort.Strings(diff.Added)
	sort.Strings(diff.Removed)
	sort.Strings(diff.Changed)
	sort.Strings(diff.RotatedSecrets)
	sort.Strings(diff.Undetectable)
	return diff
}

// rotatedMounts finds secrets present in both versions at different versions, skipping the ones an
// environment entry already reported.
func rotatedMounts(before, after []string, reported map[string]bool) []string {
	previous := map[string]string{}
	for _, ref := range before {
		secret, version, ok := splitSecretReference(ref)
		if ok {
			previous[secret] = version
		}
	}
	var out []string
	for _, ref := range after {
		secret, version, ok := splitSecretReference(ref)
		if !ok || reported[secret] {
			continue
		}
		if old, had := previous[secret]; had && old != version {
			out = append(out, secret+": "+old+" -> "+version)
		}
	}
	return out
}

// secretsNamedByEnv is the set of secrets an environment entry references, so a rotation is reported
// once — named by the variable an operator would look for.
func secretsNamedByEnv(entries []ConfigEntry) map[string]bool {
	out := map[string]bool{}
	for _, entry := range entries {
		if secret, _, ok := splitSecretReference(entry.SecretRef); ok {
			out[secret] = true
		}
	}
	return out
}

func splitSecretReference(ref string) (secret, version string, ok bool) {
	idx := strings.LastIndex(ref, "@")
	if idx <= 0 || idx == len(ref)-1 {
		return "", "", false
	}
	return ref[:idx], ref[idx+1:], true
}

func containsEntry(entries []ConfigEntry, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

// ChangeRefConfig is the deterministic ref of a configuration change: the revision the configuration
// changed **to**.
//
// The revision and not an instant, because the revision is what identifies the version — and because
// the instant a configuration version started is the instant its revision was created, which is
// already the change's valid time. Two polls observing one deploy produce one change.
func ChangeRefConfig(rev Revision) *graphv1.Ref {
	return feeder.Ref(NSChange, "config/"+rev.Service.Value()+"@"+rev.Revision)
}

// ConfigChange builds the configuration change (T136, FR-033).
//
// `at` is when the configuration became true, which is the creation instant of the revision it was
// deployed with. It is required for the reason every change instant is required: a configuration
// change with no instant is not a change, it is a claim that something changed at no particular time.
func ConfigChange(before, after ConfigObservation, diff ConfigDiff, actor Actor) (feeder.ChangeFact, error) {
	if after.CreateTime.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a configuration change for revision %s with no "+
			"instant; the instant is the revision's createTime, which is output-only and documented, "+
			"so a missing one is an error rather than an unknown start (FR-033)", after.Revision.Value())
	}
	if diff.Empty() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a configuration change for %s with an empty "+
			"diff", after.Revision.Value())
	}
	if err := after.Revision.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	kind := graphv1.ChangeKind_CONFIG_CHANGE
	if diff.OnlySecretsRotated() {
		// The more specific kind, where it is the whole of what happened. A reader asking "when was
		// this credential last rotated" should not have to read every config change to find out.
		kind = graphv1.ChangeKind_SECRET_ROTATION
	}
	return feeder.ChangeFact{
		Ref:       ChangeRefConfig(after.Revision),
		Kind:      kind,
		Summary:   "configuration of " + after.Revision.Name + " at " + after.Revision.Revision + ": " + diff.Summary(),
		ActorKind: actor.Kind,
		// The service, the revision and the configuration version are all targets, which gives the
		// change a `changed-by` edge from each: FR-033 requires one to the service and one to the
		// revision, and the configuration version is the revision's settings under their own identity.
		Targets: []*graphv1.Ref{after.Revision.Service.Ref(), after.Revision.Ref(), after.Ref()},
		ValidAt: after.CreateTime,
	}, nil
}

// ConfigChangeProps renders the configuration change's properties.
func ConfigChangeProps(before, after ConfigObservation, diff ConfigDiff, actor Actor) *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, after.Revision.Project).
		Str(PropRegion, after.Revision.Region).
		Str(PropConfigOf, after.Revision.Service.Value()).
		Str(PropConfigRevision, after.Revision.Revision).
		Str(PropConfigFrom, before.Revision.Revision).
		Str(PropActorRung, actor.Rung)
	if len(diff.Added) > 0 {
		props = props.Strs(PropConfigAddedEntries, diff.Added...)
	}
	if len(diff.Removed) > 0 {
		props = props.Strs(PropConfigRemovedEntries, diff.Removed...)
	}
	if len(diff.Changed) > 0 {
		props = props.Strs(PropConfigChangedEntries, diff.Changed...)
	}
	if len(diff.RotatedSecrets) > 0 {
		props = props.Strs(PropConfigRotatedSecrets, diff.RotatedSecrets...)
	}
	if len(diff.Undetectable) > 0 {
		// Stated on the change, not only on the node: a reader looking at "3 values changed" needs
		// to know that four more entries could not be checked.
		props = props.Str(PropConfigFingerprintsUnavailable, FingerprintsUnavailable).
			Strs("sre.gcp.config_undetectable_entries", diff.Undetectable...)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}
