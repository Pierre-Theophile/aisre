// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The disposition table (T038, FR-134, contracts/sanitisation.md §2).
//
// The contract states, per field and per label key, exactly one of three dispositions. This file
// is that table as code, and the important property is the one the zero value gives for free: a
// field nobody has assigned reads back as Unassigned, and Unassigned is an error rather than a
// silent default. There is no `default: Dropped`, which would be the safe-looking choice and the
// wrong one — a default is how the next field nobody thought about gets a disposition without a
// human ever looking at it, and half the time that default will be wrong in the direction that
// destroys the corpus (dropping a join key) rather than the direction that leaks.
//
// Labels are the one deliberate exception, and it is published rather than implicit: a label key
// that is not on the allowlist becomes nothing at all (`config/gcp.yaml` `on_unlisted: drop`),
// because a label namespace is open — anyone with deploy access invents a key — and an open
// namespace cannot be enumerated in advance. An *allowlisted* label with no disposition still
// fails, which is the case a human can actually be asked about.

// PolicyVersion is the version of contracts/sanitisation.md this policy implements. It is
// recorded in every fixture manifest (FR-134) and in every RedactionPolicy a backend declares,
// so what was applied to a recording stays knowable after the contract changes.
const PolicyVersion = "1.0.0"

// Disposition is what happens to one field or label key. Exactly one of four, and the zero value
// is the one that fails.
type Disposition uint8

// The four dispositions. Unassigned is first so that it is the zero value.
const (
	// Unassigned is a field no human has ruled on. It fails the commit (FR-134).
	Unassigned Disposition = iota
	// Verbatim records the value as it is, from the short §2.2 allowlist.
	Verbatim
	// Pseudonym replaces the value with a keyed, kind-typed HMAC (§2.3).
	Pseudonym
	// Dropped never records the value, in any form (§2.1).
	Dropped
)

// String returns the contract's own spelling, which is what a manifest and a refusal both carry.
func (d Disposition) String() string {
	switch d {
	case Verbatim:
		return "verbatim"
	case Pseudonym:
		return "pseudonym"
	case Dropped:
		return "dropped"
	default:
		return "unassigned"
	}
}

// Rule is one row of the table: the disposition, the pseudonym kind where the disposition is
// Pseudonym, and the contract's reason. The reason is carried rather than left in the document
// because it is what a refusal quotes, and a refusal that cannot say why is a refusal a
// developer routes around.
type Rule struct {
	Disposition Disposition
	Kind        Kind
	Why         string
	// Shape is the form a pseudonym takes (json.go). The zero value is the `px_` token every rule
	// before 004 T104 produced; the other shapes exist because a deploy payload carries identifiers a
	// token cannot stand in for — a numeric id a decoder reads as an integer, a commit a normaliser
	// reads as forty hex characters, a URL a mapper reads a repository out of.
	Shape Shape
	// Template is the URL or path pattern a ShapeTemplate rule rewrites, segment by segment.
	Template string
}

// UnassignedError is the refusal a field with no disposition produces. It is a distinct type
// because the recording path handles it differently from every other error: an unassigned field
// is not a transient failure to retry, it is a question for a human, and the run stops.
type UnassignedError struct {
	Field string
	Where string
}

func (e *UnassignedError) Error() string {
	where := e.Where
	if where == "" {
		where = "a payload"
	}
	return fmt.Sprintf("sanitise: field %q in %s has no disposition under policy %s; "+
		"assign it in contracts/sanitisation.md §2 and in ContractPolicy — a field with no "+
		"disposition fails the commit rather than defaulting either way (FR-134)",
		e.Field, where, PolicyVersion)
}

// Policy is the table. It is immutable once built: the dispositions a recording was made under
// must not be changeable by the code doing the recording.
type Policy struct {
	version       string
	fields        map[string]Rule
	labels        map[string]Rule
	labelUnlisted Disposition
}

// NewPolicy builds a policy over the given field and label tables. Feature 005 builds its own
// table with it, and a test builds a deliberately wrong one — which is the point: every guarantee
// this package makes about the table is enforced by Validate, so it must be possible to hand
// Validate a table that breaks one.
//
// An unlisted label key is always Dropped. It is not a parameter because there is no other
// defensible answer for a namespace nobody can enumerate, and Validate refuses anything else.
func NewPolicy(version string, fields, labels map[string]Rule) *Policy {
	p := &Policy{
		version:       version,
		labelUnlisted: Dropped,
		fields:        make(map[string]Rule, len(fields)),
		labels:        make(map[string]Rule, len(labels)),
	}
	for path, rule := range fields {
		p.fields[CanonicalField(path)] = rule
	}
	for key, rule := range labels {
		p.labels[strings.ToLower(strings.TrimSpace(key))] = rule
	}
	return p
}

// Version returns the contract version this policy implements.
func (p *Policy) Version() string { return p.version }

// arrayIndex normalises `a.b[3].c` to `a.b[].c`. Audit payloads address delegation chains and
// traffic splits by index, and a table keyed on the index would cover the first entry and miss
// the second — which is exactly the entry a delegated principal hides in.
var arrayIndex = regexp.MustCompile(`\[[0-9]+\]`)

// CanonicalField is the spelling the table is keyed on: array indices collapsed, case folded,
// surrounding space removed. A vendor spells the same path three ways across two API versions;
// the table should not have to.
func CanonicalField(path string) string {
	return arrayIndex.ReplaceAllString(strings.ToLower(strings.TrimSpace(path)), "[]")
}

// Field returns the rule for a field path, or an *UnassignedError. `where` names the artifact the
// field came from and appears in the refusal; it is for the human who has to answer the question.
func (p *Policy) Field(path, where string) (Rule, error) {
	key := CanonicalField(path)
	if rule, ok := p.fields[key]; ok && rule.Disposition != Unassigned {
		return rule, nil
	}
	// A path the table does not name by itself may still be covered by a prefix rule: the
	// contract assigns whole subtrees (`protopayload.request` is configuration, all of it), and
	// enumerating a vendor's request schema field by field is a table that is wrong by the next
	// API version.
	if rule, ok := p.prefixRule(key); ok {
		return rule, nil
	}
	return Rule{}, &UnassignedError{Field: path, Where: where}
}

// prefixRule finds the longest subtree rule covering key. Longest wins so that a specific field
// inside a dropped subtree can still be allowlisted — the API method name inside a request body
// is the case that matters, since FR-039 requires it be recorded.
func (p *Policy) prefixRule(key string) (Rule, bool) {
	best := ""
	var found Rule
	for prefix, rule := range p.fields {
		if !strings.HasSuffix(prefix, ".*") {
			continue
		}
		stem := strings.TrimSuffix(prefix, "*")
		if strings.HasPrefix(key, stem) && len(stem) > len(best) {
			best, found = stem, rule
		}
	}
	return found, best != ""
}

// Label returns the rule for one label key. A key that is not on the allowlist becomes nothing:
// the published open-namespace rule, not a default (see the file comment).
func (p *Policy) Label(key string) (Rule, error) {
	normalised := strings.ToLower(strings.TrimSpace(key))
	rule, listed := p.labels[normalised]
	if !listed {
		return Rule{Disposition: p.labelUnlisted, Why: "not on the label allowlist (config/gcp.yaml)"}, nil
	}
	if rule.Disposition == Unassigned {
		return Rule{}, &UnassignedError{Field: "labels." + key, Where: "the label allowlist"}
	}
	return rule, nil
}

// Fields returns the field paths carrying disposition d, sorted. It is how a RedactionPolicy's
// dropped_fields and pseudonymised_fields are populated, so the declaration a backend publishes
// cannot drift from the table it actually applies.
func (p *Policy) Fields(d Disposition) []string {
	out := make([]string, 0, len(p.fields))
	for path, rule := range p.fields {
		if rule.Disposition == d {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Validate refuses a policy that is internally inconsistent. Three of the four checks exist
// because a plausible future edit would otherwise pass review:
//
//   - a Pseudonym rule with no kind would collide a service and an instance that share a name
//     into one token, silently merging two entities in the recorded graph (§2.3 property 2);
//   - a kind on a rule that is not a Pseudonym is a rule somebody half-changed;
//   - a people field assigned anything but Dropped is FR-135 violated in the table itself, which
//     is the one place no scan downstream would look. This is the check that makes "dropped,
//     never hashed" structural rather than a habit.
func (p *Policy) Validate() error {
	if p.version == "" {
		return fmt.Errorf("sanitise: a policy with no version; a recording must say what was applied to it")
	}
	for _, table := range []map[string]Rule{p.fields, p.labels} {
		for path, rule := range table {
			switch {
			case rule.Disposition == Unassigned:
				return &UnassignedError{Field: path, Where: "the policy table"}
			case rule.Disposition == Pseudonym && rule.Kind == "":
				return fmt.Errorf("sanitise: %s is pseudonymised with no kind; an untyped pseudonym "+
					"merges a service and an instance that share a name (contract §2.3)", path)
			case rule.Disposition != Pseudonym && rule.Kind != "":
				return fmt.Errorf("sanitise: %s is %s but names pseudonym kind %q", path, rule.Disposition, rule.Kind)
			case IsPeopleField(path) && rule.Disposition != Dropped:
				return fmt.Errorf("sanitise: %s names a person but is %s; people identifiers are "+
					"dropped, never hashed — a hashed principal address is still personal data "+
					"(FR-135, SC-019)", path, rule.Disposition)
			case rule.Disposition != Pseudonym && (rule.Shape != ShapeToken || rule.Template != ""):
				return fmt.Errorf("sanitise: %s is %s but names a pseudonym shape", path, rule.Disposition)
			case rule.Shape == ShapeTemplate:
				if _, err := compileTemplate(rule.Template); err != nil {
					return err
				}
			case rule.Template != "":
				return fmt.Errorf("sanitise: %s carries a template but is not ShapeTemplate", path)
			}
		}
	}
	if p.labelUnlisted != Dropped {
		return fmt.Errorf("sanitise: an unlisted label key is %s; config/gcp.yaml publishes drop, "+
			"and a label namespace nobody can enumerate cannot have any other answer", p.labelUnlisted)
	}
	return nil
}

// ContractPolicy returns the published table of contracts/sanitisation.md §2 at PolicyVersion.
//
// A row's Why is the contract's own reason, not a restatement of the disposition. Where a row
// departs from the obvious reading of the contract, the reason says so — those are the rows a
// reviewer should argue with, and they are the rows a reviewer can actually find.
func ContractPolicy() *Policy {
	p := &Policy{
		version:       PolicyVersion,
		labelUnlisted: Dropped,
		fields:        map[string]Rule{},
		labels:        map[string]Rule{},
	}
	drop := func(path, why string) { p.fields[CanonicalField(path)] = Rule{Disposition: Dropped, Why: why} }
	keep := func(path, why string) { p.fields[CanonicalField(path)] = Rule{Disposition: Verbatim, Why: why} }
	pseudo := func(path string, kind Kind, why string) {
		p.fields[CanonicalField(path)] = Rule{Disposition: Pseudonym, Kind: kind, Why: why}
	}

	// --- §2.1 dropped: never recorded, in any form -------------------------------------------

	// The principal, and every delegation entry beside it. The delegation chain is why the path
	// is normalised on array index: a table keyed on `[0]` would cover the direct principal and
	// miss the impersonated one, which is the entry that names the human behind a service
	// account.
	drop("protoPayload.authenticationInfo.principalEmail", "a principal email address")
	drop("protoPayload.authenticationInfo.principalSubject", "a principal, in subject form")
	drop("protoPayload.authenticationInfo.serviceAccountKeyName", "names a key, and by convention its owner")
	drop("protoPayload.authenticationInfo.serviceAccountDelegationInfo[].firstPartyPrincipal.principalEmail",
		"the human behind an impersonated service account")
	drop("protoPayload.authenticationInfo.serviceAccountDelegationInfo[].thirdPartyPrincipal.*",
		"an external principal, shape unbounded")
	drop("protoPayload.authenticationInfo.thirdPartyPrincipal.*", "an external principal, shape unbounded")
	drop("protoPayload.requestMetadata.callerSuppliedUserAgent", "free text a client chose")

	// A caller IP is deliberately *not* treated as the infrastructure address FR-135 lists. An
	// infrastructure IP names a machine that serves traffic; `callerIp` names where a person was
	// sitting when they deployed, and pseudonymising it would keep a stable, linkable token for
	// one human's home connection. It is dropped.
	drop("protoPayload.requestMetadata.callerIp", "where a person was sitting, not a machine that serves traffic")
	drop("protoPayload.requestMetadata.callerNetwork", "the same, one level up")

	// A request body is the configuration an operator applied: environment variables, connection
	// strings, secret references. FR-034 classifies configuration values sensitive, and the
	// subtree is dropped rather than enumerated, because a table that enumerates a vendor's
	// request schema is a table that is wrong by the next API version.
	drop("protoPayload.request.*", "the applied configuration; sensitive by FR-034 and unbounded in shape")
	drop("protoPayload.response.*", "the returned object, same reasoning")
	drop("protoPayload.status.message", "free text from the vendor")
	drop("protoPayload.metadata.*", "vendor-defined, unbounded")

	// Vendor notices. The body never reaches disk, a log or any artifact, including on a failed
	// run (FR-071, FR-137) — which is why this is a disposition and not a step the recorder is
	// trusted to remember.
	drop("message.sender", "a message sender address (FR-072)")
	drop("message.from", "a message sender address (FR-072)")
	drop("message.to[]", "a message recipient address (FR-072)")
	drop("message.cc[]", "a message recipient address (FR-072)")
	drop("message.bcc[]", "a message recipient address (FR-072)")
	drop("message.replyTo", "a message address (FR-072)")
	drop("message.body", "a message body: never to the graph, disk, a log or any artifact (FR-071)")
	drop("message.bodyHtml", "a message body (FR-071)")
	drop("message.snippet", "a body by another name (FR-071)")
	drop("message.subject", "a subject may be summarised into typed fields; it is not stored as itself")
	drop("message.quotedThread", "a quoted thread is somebody else's body")
	drop("message.attachments[].*", "attachments are not opened in v1")

	// Raw log payloads. A recorded entry is a mined template with its variables masked (§3); the
	// raw line has no route out of the process.
	drop("logEntry.textPayload", "a raw log line; recordings carry mined templates (FR-136, FR-090)")
	drop("logEntry.jsonPayload.*", "a structured log body, unbounded and customer-bearing")
	drop("logEntry.protoPayload.*", "a structured log body, unbounded and customer-bearing")
	drop("customer_id", "a customer identifier appearing in a log sample")
	drop("end_user_id", "a customer identifier appearing in a log sample")
	drop("secret_value", "secret material (FR-034)")
	drop("env[].value", "a configuration value; classified sensitive by FR-034")

	// Vercel's environment-variable metadata (004 T095). `env[].value` above is GCP's spelling and
	// covers none of these: the response is `{"envs":[…]}` and it carries the secret in THREE places,
	// all of them REQUIRED or routinely present. `value` is listed among the endpoint's required
	// response fields, `legacyValue` is documented as "Legacy now-encryption ciphertext", and
	// `internalContentHint.encryptedValue` as "Contains the `value` of the env variable, encrypted with
	// a special key". FR-038 refuses the value plaintext, ciphertext, truncated or hashed, so all three
	// are dropped and the ciphertexts are dropped by the same clause as the plaintext.
	//
	// This is a disposition rather than something the recorder is trusted to remember, for the reason
	// the vendor-notice block above gives: the connector's own struct has no field for any of them, so
	// nothing reaches the GRAPH, but `--record` writes the response bytes and those are a different
	// path to disk (FR-062, FR-137).
	drop("envs[].value", "an environment variable's value (FR-038)")
	drop("envs[].legacyValue", "the same value as legacy ciphertext (FR-038: not ciphertext either)")
	drop("envs[].internalContentHint.*", "documented as carrying the encrypted value; subtree, because a "+
		"table that enumerates a vendor's hint schema is wrong by the next API version")

	// --- The deploy feeders' payloads, as their live pollers write them (004 T104, T106, FR-063) -----
	//
	// Every row is rooted at `github.<payload kind>` or `vercel.<payload kind>`, which is what
	// Sanitiser.JSON addresses a payload's fields by, so none of them can answer for a GCP field.
	//
	// The rules, from FR-063. People are dropped: logins, account ids, a variable's editor. The actor
	// KIND is kept verbatim: GitHub's `type` and Vercel's `creator.type` are the platform typing the
	// account, which is the evidence FR-027 derives PERSON / AUTOMATION from, and they name nobody.
	// Repository, organisation, project, deployment, run and release identifiers are pseudonymised,
	// each in the SHAPE its field has: a numeric id becomes digits, a commit becomes hex, a URL is
	// rewritten segment by segment (json.go). Free text is dropped: a branch name, a release title, a
	// poll's failure reason, which quotes URLs.
	//
	// Kept verbatim and argued: an environment name and a workflow name are what the operator's
	// allowlist matches (FR-023, FR-024) — a pseudonymised `production` is a recording that replays
	// to nothing — and they are the operator's configuration rather than anybody's identity. A
	// release tag is the `deploy.release` key, and a version string names a build. A variable's KEY
	// is the configuration change's subject; its value is dropped above, in all three forms.
	shape := func(path string, kind Kind, sh Shape, why string) {
		p.fields[CanonicalField(path)] = Rule{Disposition: Pseudonym, Kind: kind, Shape: sh, Why: why}
	}
	url := func(path, pattern, why string) {
		p.fields[CanonicalField(path)] = Rule{Disposition: Pseudonym, Kind: KindRepository, Shape: ShapeTemplate, Template: pattern, Why: why}
	}
	const (
		ghDeployment = "https://api.github.com/repos/{organisation}/{repository}/deployments/{revision:digits}"
		ghRepo       = "{organisation}/{repository}"
		whyCommit    = "a commit; hex so the claim normaliser still reads it and C8 still joins on it"
	)
	for _, account := range []string{
		"github.deployments[].creator", "github.deployment-statuses.statuses[].creator",
		"github.workflow-runs.workflow_runs[].actor", "github.workflow-runs.workflow_runs[].triggering_actor",
		"github.releases.releases[].author",
	} {
		drop(account+".login", "a person's login (FR-063)")
		drop(account+".id", "a person's account id: an id that identifies one person is a people identifier")
		keep(account+".type", "the platform typing the account; the evidence FR-027 derives the actor kind from")
	}
	keep("github.installation-repositories.total_count", "a count")
	keep("github.installation-repositories.repository_selection", "the grant's regime, `all` or `selected` (FR-008)")
	shape("github.installation-repositories.repositories[].id", KindRepository, ShapeDigits, "the id every change identity is built from")
	shape("github.installation-repositories.repositories[].name", KindRepository, ShapeToken, "a repository name")
	shape("github.installation-repositories.repositories[].owner.login", KindOrganisation, ShapeToken, "the installation's organisation")
	drop("github.installation-repositories.repositories[].full_name", "restates owner and name; the people vocabulary drops it anyway")
	keep("github.installation-repositories.repositories[].private", "a flag")

	shape("github.deployments[].id", KindRevision, ShapeDigits, "a deployment id; joined to its statuses and its URL")
	shape("github.deployments[].sha", KindCommit, ShapeHex, whyCommit)
	drop("github.deployments[].ref", "a branch name: free text, and people put their names in branches")
	keep("github.deployments[].task", "`deploy` or the operator's own task name")
	keep("github.deployments[].environment", "what the operator's allowlist matches (FR-023)")
	keep("github.deployments[].production_environment", "a flag")
	keep("github.deployments[].transient_environment", "a flag")
	keep("github.deployments[].created_at", "an instant")
	keep("github.deployments[].updated_at", "an instant")
	url("github.deployments[].url", ghDeployment, "where GitHub states the deployment's repository")
	url("github.deployments[].statuses_url", ghDeployment+"/statuses", "the same, for the statuses")

	url("github.deployment-statuses.repository", ghRepo, "the repository the statuses belong to")
	shape("github.deployment-statuses.deployment_id", KindRevision, ShapeDigits, "the deployment the statuses belong to")
	shape("github.deployment-statuses.statuses[].id", KindStatus, ShapeDigits, "a status id")
	keep("github.deployment-statuses.statuses[].state", "the transition, from a published vocabulary")
	keep("github.deployment-statuses.statuses[].environment", "what the operator's allowlist matches")
	keep("github.deployment-statuses.statuses[].created_at", "the instant a rollout is dated by")
	keep("github.deployment-statuses.statuses[].updated_at", "an instant")
	drop("github.deployment-statuses.statuses[].log_url", "set by the deployer, any host, and where tokens arrive (FR-014)")
	drop("github.deployment-statuses.statuses[].environment_url", "set by the deployer, any host")
	drop("github.deployment-statuses.statuses[].target_url", "set by the deployer, any host")

	keep("github.workflow-runs.total_count", "a count")
	shape("github.workflow-runs.workflow_runs[].id", KindRun, ShapeDigits, "a run id; joined to its URLs")
	keep("github.workflow-runs.workflow_runs[].name", "what the operator's deploy-workflow allowlist matches (FR-024)")
	keep("github.workflow-runs.workflow_runs[].run_number", "a counter")
	keep("github.workflow-runs.workflow_runs[].run_attempt", "a counter")
	shape("github.workflow-runs.workflow_runs[].head_sha", KindCommit, ShapeHex, whyCommit)
	keep("github.workflow-runs.workflow_runs[].event", "the trigger, from a published vocabulary; actor-kind evidence")
	keep("github.workflow-runs.workflow_runs[].status", "a published vocabulary")
	keep("github.workflow-runs.workflow_runs[].conclusion", "a published vocabulary")
	keep("github.workflow-runs.workflow_runs[].created_at", "an instant")
	keep("github.workflow-runs.workflow_runs[].run_started_at", "an instant")
	keep("github.workflow-runs.workflow_runs[].updated_at", "an instant")
	url("github.workflow-runs.workflow_runs[].html_url", "https://github.com/{organisation}/{repository}/actions/runs/{run:digits}", "the run's page, the origin link")
	url("github.workflow-runs.workflow_runs[].logs_url", "https://api.github.com/repos/{organisation}/{repository}/actions/runs/{run:digits}/logs", "a link to the logs, never their content")

	url("github.releases.repository", ghRepo, "the repository the releases belong to")
	shape("github.releases.releases[].id", KindRelease, ShapeDigits, "a release id")
	keep("github.releases.releases[].tag_name", "the `deploy.release` key; a version string names a build")
	drop("github.releases.releases[].name", "a release title: free text")
	shape("github.releases.releases[].target_commitish", KindCommit, ShapeHex, "a commit where it is one; a branch name is dropped by the shape")
	keep("github.releases.releases[].draft", "a flag")
	keep("github.releases.releases[].prerelease", "a flag")
	keep("github.releases.releases[].created_at", "an instant")
	keep("github.releases.releases[].published_at", "the instant a release is dated by")
	drop("github.releases.releases[].html_url", "carries the tag inside a URL; the release is findable from its id")

	keep("github.poll-marker.outcome", "`complete` or `partial` (FR-056)")
	drop("github.poll-marker.reason", "an error message, which quotes URLs and repository names")

	shape("vercel.project.id", KindProject, ShapeToken, "a project id; joined to its deployments and its variables")
	shape("vercel.project.name", KindProject, ShapeToken, "a project name; joined to the deployments that state it")
	keep("vercel.project.createdat", "an instant")
	keep("vercel.project.link.type", "`github`, `gitlab` or `bitbucket`")
	shape("vercel.project.link.org", KindOrganisation, ShapeToken, "the connected repository's organisation; the same token GitHub's owner gets")
	shape("vercel.project.link.repo", KindRepository, ShapeToken, "the connected repository; the same token GitHub's name gets")
	shape("vercel.project.link.repoid", KindRepository, ShapeDigits, "the connected repository's id; the same digits GitHub's id gets")
	keep("vercel.project.lastaliasrequest", "absent (null) when the project has had no alias request")
	keep("vercel.project.lastaliasrequest.type", "`rollback` or `promote`")
	keep("vercel.project.lastaliasrequest.jobstatus", "a published vocabulary")
	shape("vercel.project.lastaliasrequest.fromdeploymentid", KindRevision, ShapeToken, "the deployment a rollback left; the token its rollout carries (004 T155)")
	shape("vercel.project.lastaliasrequest.todeploymentid", KindRevision, ShapeToken, "the deployment production moved to")
	keep("vercel.project.lastaliasrequest.requestedat", "an instant")

	shape("vercel.project-env.projectid", KindProject, ShapeToken, "whose configuration this is")
	shape("vercel.project-env.envs[].id", KindResource, ShapeToken, "a variable's id")
	keep("vercel.project-env.envs[].key", "the configuration change's subject; the value is dropped above, in all three forms")
	keep("vercel.project-env.envs[].target[]", "a published vocabulary")
	keep("vercel.project-env.envs[].type", "a published vocabulary")
	keep("vercel.project-env.envs[].system", "a flag")
	keep("vercel.project-env.envs[].createdat", "the instant a creation is dated by")
	keep("vercel.project-env.envs[].updatedat", "the instant an edit is dated by")

	shape("vercel.deployments.deployments[].uid", KindRevision, ShapeToken, "a deployment id; the rollback rows above join to it")
	shape("vercel.deployments.deployments[].name", KindProject, ShapeToken, "the project's name as the deployment states it")
	shape("vercel.deployments.deployments[].projectid", KindProject, ShapeToken, "the project the deployment belongs to")
	for _, field := range []string{"target", "readystate", "readysubstate", "source", "isrollbackcandidate", "createdat", "buildingat", "ready"} {
		keep("vercel.deployments.deployments[]."+field, "a published vocabulary, a flag or an instant")
	}
	p.fields[CanonicalField("vercel.deployments.deployments[].inspectorurl")] = Rule{
		Disposition: Pseudonym, Kind: KindProject, Shape: ShapeTemplate,
		Template: "https://vercel.com/{team}/{project}/{revision}", Why: "the origin link",
	}
	drop("vercel.deployments.deployments[].creator.uid", "a person's account id")
	keep("vercel.deployments.deployments[].creator.type", "the platform typing the account (FR-037)")
	for _, meta := range []string{"vercel.deployments.deployments[].meta", "vercel.deployments.deployments[].attribution.commitmeta"} {
		keep(meta, "absent (null) where the poller kept no key")
		for _, key := range []string{"githubcommitsha", "gitlabcommitsha", "bitbucketcommitsha", "commitsha"} {
			shape(meta+"."+key, KindCommit, ShapeHex, whyCommit)
		}
		keep(meta+".promotedat", "the instant a promotion is dated by, where stated")
	}
	keep("vercel.poll-marker.outcome", "`complete` or `partial`")
	drop("vercel.poll-marker.reason", "an error message, which quotes project names")
	// --- The Datadog feeder's payloads (005 T080) ---------------------------------------------------
	//
	// Rooted at `datadog.<payload kind>`. They reach this table after internal/feeders/datadog's
	// pre-pass (recording.go), which keeps only the fields the feeder reads and rewrites every identifier
	// inside free text — a query's `service:…`, a group key's `env:…`, a tag's `team:…` — with the keyed
	// pseudonym for its kind, so the query, the tags and the group keys are kept here AS REWRITTEN. The
	// monitor's notification message, its creator, options and roles never get this far.
	keep("datadog.monitors[].id", "an opaque counter Datadog assigns; it names no person, service or host, "+
		"and the query pointer executes against it")
	pseudo("datadog.monitors[].name", KindAlertPolicy, "a monitor's name, which routinely names the service")
	keep("datadog.monitors[].type", "a published vocabulary")
	keep("datadog.monitors[].query", "the monitor's query, its identifiers rewritten by the pre-pass (FR-074)")
	keep("datadog.monitors[].tags[]", "identifier tags only, rewritten by the pre-pass; any other tag was dropped there")
	keep("datadog.monitors[].tags", "absent (null) when no identifier tag survived the pre-pass")
	keep("datadog.monitors[].priority", "a number from 1 to 5")
	keep("datadog.monitors[].overall_state", "a published vocabulary")
	keep("datadog.monitors[].created", "an instant")
	keep("datadog.monitors[].modified", "the instant an edit is dated by")
	keep("datadog.monitors[].state.groups.*", "per-group status and Datadog's stated instants; the group keys "+
		"were rewritten by the pre-pass")
	keep("datadog.discovery.log_sources[]", "watched sources, pseudonymised by the pre-pass")
	keep("datadog.discovery.window.*", "the window measured")
	keep("datadog.discovery.measurements[].source", "pseudonymised by the pre-pass")
	for _, count := range []string{"lines", "error_lines", "host_lines"} {
		keep("datadog.discovery.measurements[]."+count, "a count of lines where a field is present, never of an event")
	}
	keep("datadog.discovery.measurements[].candidates[].*", "a published convention's label and presence counts")
	keep("datadog.discovery.measurements[].values[].value", "a deployed version (see `version`); the join to the "+
		"deploy feeders' changes")
	keep("datadog.discovery.measurements[].values[].first_seen", "an instant")
	keep("datadog.discovery.measurements[].values[].beyond_horizon", "a flag")
	keep("datadog.discovery.measurements[].tags[].key", "an allowlisted tag key")
	keep("datadog.discovery.measurements[].tags[].value", "pseudonymised by the pre-pass")
	keep("datadog.discovery.measurements[].tags[].lines", "a count")
	keep("datadog.discovery.measurements[].env_field", "a field name from the published environment-field list")
	keep("datadog.discovery.measurements[].env_discovery.service_lines", "a count")
	keep("datadog.discovery.measurements[].env_discovery.fields[].*", "a published field's label and a count")
	keep("datadog.discovery.measurements[].env_discovery.values[].value", "an environment, pseudonymised by the pre-pass")
	keep("datadog.discovery.measurements[].env_discovery.values[].lines", "a count")
	keep("datadog.discovery.measurements[].failed", "a fixed sentence; the vendor's reason is withheld by the pre-pass")
	keep("datadog.discovery.measurements[].values_failed", "a fixed sentence; the vendor's reason is withheld by the pre-pass")
	keep("datadog.poll.outcome", "`complete` or `partial`")
	keep("datadog.poll.pages", "a count")
	drop("datadog.poll.reason", "an error message, which quotes URLs and names")
	keep("datadog.poll.stop_reason", "`quota` or `rate_limited`, the connector's own typed stop")
	keep("datadog.poll.deferred[]", "area names from the published deferral order")
	keep("datadog.poll.resume_at", "the instant Datadog's Retry-After ends")
	keep("datadog.poll.usage", "the connector's usage report: its area names, Datadog's rate-limit bucket names "+
		"(X-RateLimit-Name, a published vocabulary) and counts")

	// The `changes` capability's payloads (005 T092). The pre-pass (recording.go, prepareEvents) keeps only
	// the fields the feeder reads from an event, drops the title and the event's name (free text), rewrites
	// the tags it keeps and drops the tags that name the actor: a people identifier does not survive in any
	// form, so the actor's NAME is absent from a recording and only the actor kind survives, derived from
	// the trigger tag and the source.
	keep("datadog.events.data[].id", "an opaque identifier Datadog assigns to the event; it names no person, "+
		"service or host, and the change's ref and idempotency key are built from it")
	keep("datadog.events.data[].type", "a published vocabulary (`event`)")
	keep("datadog.events.data[].attributes.timestamp", "the instant Datadog recorded, which dates the change")
	keep("datadog.events.data[].attributes.tags[]", "evidence tags only, rewritten by the pre-pass: identifiers "+
		"pseudonymised, actor tags dropped, any tag the feeder does not read dropped")
	keep("datadog.events.data[].attributes.tags", "absent (null) when no tag survived the pre-pass")
	keep("datadog.events.data[].attributes.attributes.source_type_name", "an event source from the published "+
		"actor vocabulary, or the organisation's own word pseudonymised by the pre-pass")
	keep("datadog.events.data[].attributes.attributes.service", "a service name, pseudonymised by the pre-pass "+
		"with the same token the service tag gets, so the event still names the service the logs do")
	keep("datadog.events.data[].attributes.attributes.evt.id", "an opaque identifier Datadog assigns")
	keep("datadog.events.data[].attributes.attributes.evt.type", "a kind from the published change taxonomy, or "+
		"the organisation's own word pseudonymised by the pre-pass")
	keep("datadog.events-poll.outcome", "`complete` or `partial`")
	keep("datadog.events-poll.window.*", "the window read")
	for _, count := range []string{"pages", "read", "out_of_scope"} {
		keep("datadog.events-poll."+count, "a count")
	}
	drop("datadog.events-poll.reason", "an error message, which quotes URLs and names")
	keep("datadog.events-poll.stop_reason", "`quota` or `rate_limited`, the connector's own typed stop")
	keep("datadog.events-poll.deferred[]", "area names from the published deferral order")
	keep("datadog.events-poll.resume_at", "the instant Datadog's Retry-After ends")
	keep("datadog.events-poll.usage", "the connector's usage report: its area names, Datadog's rate-limit "+
		"bucket names and counts")

	// The `apm_topology` capability's payload (005 T090). The pre-pass (recording.go, prepareTopology)
	// pseudonymises every environment, service, host and operation name with the keyed pseudonym for its
	// kind, so the recorded edges join the recorded nodes and the recorded log sources; a version is kept,
	// for the join to the deploy feeders' changes; the vendor's failure reason is withheld.
	keep("datadog.topology.env", "an environment, pseudonymised by the pre-pass")
	keep("datadog.topology.window.*", "the window read; an edge's valid interval")
	for _, part := range []string{"dependencies", "traffic", "versions", "operations", "hosts"} {
		keep("datadog.topology.parts."+part, "`complete`, `partial` or `unread`")
	}
	keep("datadog.topology.services[].name", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.services[].calls[]", "a callee, pseudonymised by the pre-pass like a service")
	keep("datadog.topology.services[].calls", "absent (null) when the service calls nothing")
	keep("datadog.topology.traffic[].caller", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.traffic[].callee", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.traffic[].hits", "a count of spans, turned into a weight class and never stored")
	keep("datadog.topology.versions[].service", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.versions[].version", "a deployed version (see `version`); the join to the deploy feeders' changes")
	keep("datadog.topology.versions[].hits", "a count of spans that chooses the dominant version")
	keep("datadog.topology.operations[].service", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.operations[].operation", "an operation name, pseudonymised by the pre-pass as a resource")
	keep("datadog.topology.operations[].hits", "a count of spans that chooses the busiest operation")
	keep("datadog.topology.hosts[].service", "a service, pseudonymised by the pre-pass")
	keep("datadog.topology.hosts[].host", "a host, pseudonymised by the pre-pass; never a container or a pod")
	drop("datadog.topology.reason", "an error message, which quotes URLs and names")
	keep("datadog.topology.stop_reason", "`quota` or `rate_limited`, the connector's own typed stop")
	keep("datadog.topology.deferred[]", "area names from the published deferral order")
	keep("datadog.topology.resume_at", "the instant Datadog's Retry-After ends")
	keep("datadog.topology.usage", "the connector's usage report: its area names, Datadog's rate-limit bucket names and counts")

	drop("envs[].contentHint.*", "vendor-defined and unbounded, and it names backing stores")
	drop("envs[].comment", "free text somebody wrote next to a secret, which is where a value gets pasted")
	drop("envs[].edgeConfigTokenId", "a token identifier")
	drop("envs[].sunsetSecretId", "a secret identifier from the migration off `type: secret`")
	drop("description", "a free-text field not on the §2.2 allowlist (FR-136)")
	drop("annotation", "a free-text field not on the §2.2 allowlist (FR-136)")
	drop("documentation.content", "an alert policy's prose: what the injection barrier cannot bound")
	drop("free_text", "a free-text field not on the §2.2 allowlist (FR-136)")

	// --- §2.2 verbatim: the allowlist, and it is short ----------------------------------------

	keep("region", "a closed, public vocabulary of GCP region names")
	keep("resource.labels.location", "a GCP region or zone name")
	keep("resource.labels.region", "a GCP region name")
	keep("database.version", "a public product identifier (POSTGRES_16)")
	keep("settings.tier", "a public machine-type identifier")
	keep("protoPayload.methodName", "public API surface; FR-039 requires the operation name for other-kind changes")
	keep("protoPayload.serviceName", "a public API host name (run.googleapis.com)")
	keep("protoPayload.authorizationInfo[].permission", "a published IAM permission name")
	keep("protoPayload.authorizationInfo[].granted", "a boolean about structure")
	keep("change.kind", "a published closed enumeration")
	keep("change.announcement_state", "a published closed enumeration")
	// The rollback marker is a boolean about structure, and it is on the allowlist for the same
	// reason `authorizationInfo[].granted` is: there is no value a boolean could carry that names
	// anybody. Recording it as a pseudonym would be worse than useless — `px_res_k3m2` where `true`
	// belongs makes a corpus that cannot test FR-016's own clause, that a rollback is flagged.
	keep("change.rollback", "a boolean about structure (FR-016)")

	// Vercel's environment-variable metadata, the half that is NOT the value (004 T095). Every field
	// the connector reads needs a row: a field with no disposition reads back as Unassigned and FAILS
	// the commit gate, so the first recording carrying one would be refused mid-campaign with nobody
	// able to answer "what should this be?" (the T140 episode, repeated).
	//
	// The KEY is kept verbatim, and it is the only judgement here worth arguing. It can carry an
	// estate's vocabulary — `ACME_BILLING_URL` names a company — and a corpus that pseudonymised it
	// could not test FR-038 at all, since the key IS what a configuration change asserts. The same
	// trade as `change.rollback`: a pseudonym where the fact belongs makes a corpus that cannot test
	// the clause. The safeguard is not this row but Sanitiser.Field, which drops a verbatim value
	// carrying a person whatever the table says.
	keep("envs[].key", "the variable's name: what FR-038 records, and what a corpus needs to test it")
	keep("envs[].type", "a closed vocabulary (encrypted, plain, secret, sensitive, system)")
	// Both spellings, because the published schema makes `target` a `oneOf`: an array of environment
	// names or a single one. A table that covered only the array would refuse the scalar form the first
	// time a variable was scoped to one environment.
	keep("envs[].target[]", "a closed vocabulary of environment names (production, preview, development)")
	keep("envs[].target", "the same field in its scalar form")
	keep("envs[].system", "a boolean about structure: did the platform assign this variable")
	keep("envs[].decrypted", "a boolean about structure, and always false on this connector's reads")
	keep("envs[].visibility", "a closed vocabulary (config, secret)")
	keep("envs[].securityIssues[]", "a closed vocabulary of the platform's own warnings")
	keep("envs[].createdAt", "an instant")
	keep("envs[].updatedAt", "an instant")
	pseudo("envs[].id", KindResource, "an infrastructure identifier that must join across the corpus")
	pseudo("envs[].configurationId", KindResource, "an infrastructure identifier")
	pseudo("envs[].edgeConfigId", KindResource, "an infrastructure identifier")
	pseudo("envs[].customEnvironmentIds[]", KindEnvironment, "an infrastructure identifier")
	// A git branch is not free text and not quite an identifier: `jane/hotfix` names a person as
	// often as it names a change. Pseudonymised rather than kept, and the people rule still runs over
	// it first.
	pseudo("envs[].gitBranch", KindResource, "a branch name, which names a person as often as a change")
	keep("actor.kind", "a published closed enumeration")
	keep("alert.state", "a published closed enumeration")
	keep("outcome", "a published closed enumeration")
	keep("failure_reason", "a published closed enumeration")
	keep("cost_class", "a published closed enumeration")
	keep("environment_derived", "the derived environment is the closed published enum, not the label it came from")
	keep("traffic[].percent", "a number about structure, not about people")
	keep("count", "a number about structure")
	keep("duration_seconds", "a number about structure")
	keep("statistic", "a number about structure")
	keep("vendor.slug", "configuration the organisation authored, from the allowlist")
	keep("vendor.product", "configuration the organisation authored, from the allowlist")
	keep("pointer.vocabulary", "a published identifier")
	keep("pointer.vocabulary_version", "a published identifier")
	keep("logEntry.template", "a mined template; §3 masking is a precondition asserted separately, not here")
	keep("logEntry.severity", "a closed vocabulary")

	// Timestamps are not sanitised, and §4 says why: shifting them would destroy the one thing
	// the corpus exists to test, and valid-time exactness is asserted to the instant by SC-002
	// and SC-004. The disclosure is accepted and signed off under §6.
	for _, ts := range []string{
		"timestamp", "receiveTimestamp", "createTime", "updateTime", "deleteTime",
		"valid_from", "valid_to", "observed_from", "observed_to",
	} {
		keep(ts, "an instant, recorded as it is: §4's deliberate decision")
	}

	// --- §2.3 keyed pseudonym: infrastructure identifiers -------------------------------------

	pseudo("project_id", KindProject, "an infrastructure identifier that must join across the corpus")
	pseudo("project_number", KindProject, "the same identifier in numeric form; pseudonymising one and not the other joins them back together")
	pseudo("resource.labels.project_id", KindProject, "an infrastructure identifier")
	pseudo("resource.labels.service_name", KindService, "an infrastructure identifier")
	pseudo("resource.labels.revision_name", KindRevision, "an infrastructure identifier")
	pseudo("resource.labels.configuration_name", KindResource, "an infrastructure identifier")
	pseudo("resource.labels.database_id", KindInstance, "an infrastructure identifier")
	pseudo("resource.labels.instance_id", KindInstance, "an infrastructure identifier")
	pseudo("resource.labels.cluster_name", KindCluster, "an infrastructure identifier")
	pseudo("resource.labels.namespace_name", KindNamespace, "an infrastructure identifier")
	pseudo("service", KindService, "an infrastructure identifier")
	pseudo("service_name", KindService, "an infrastructure identifier")
	pseudo("otel.service.name", KindService, "the declared OpenTelemetry service name; FR-118 resolves on its equality, which a consistent pseudonym preserves")
	pseudo("revision", KindRevision, "an infrastructure identifier")
	pseudo("revision_name", KindRevision, "an infrastructure identifier")
	pseudo("instance", KindInstance, "an infrastructure identifier")
	pseudo("instance_connection_name", KindInstance, "carries project, region and instance in one string")
	pseudo("cluster", KindCluster, "an infrastructure identifier")
	pseudo("namespace", KindNamespace, "an infrastructure identifier")
	pseudo("host", KindHost, "an infrastructure identifier")
	pseudo("hostname", KindHost, "an infrastructure identifier")
	pseudo("ip", KindIPAddress, "a machine that serves traffic, unlike callerIp above")
	pseudo("ip_address", KindIPAddress, "a machine that serves traffic")
	pseudo("ipAddresses[].ipAddress", KindIPAddress, "a Cloud SQL instance address")
	pseudo("team", KindTeam, "an infrastructure identifier; FR-124 builds OWNER nodes from it")
	pseudo("name", KindResource, "a resource name")
	pseudo("resource_name", KindResource, "a resource name")
	pseudo("protoPayload.resourceName", KindResource, "carries project, region and the changed resource")
	pseudo("protoPayload.request.name", KindResource, "the changed resource, allowlisted back out of the dropped request subtree")
	pseudo("selfLink", KindResource, "a resource name in URL form")
	// The identity a workload runs as. It determines what the workload can reach, which makes it
	// structure and a cause of a permission failure — and it is a robot, not a person: the domain is
	// Google-managed and no human has an address at it. It is pseudonymised rather than dropped
	// because a revision and the database it can reach are joined through it.
	pseudo("service_account", KindResource, "the robot identity a workload runs as; infrastructure, not a person")
	pseudo("serviceAccountEmail", KindResource, "the same identity as another API spells it")
	pseudo("alert_policy_id", KindAlertPolicy, "an infrastructure identifier")
	pseudo("alertPolicy.name", KindAlertPolicy, "an infrastructure identifier")
	pseudo("incident.policy_name", KindAlertPolicy, "an infrastructure identifier")
	pseudo("load_balancer", KindLoadBalancer, "an infrastructure identifier")
	pseudo("dns_record", KindDNSRecord, "an infrastructure identifier")
	// The deployment or version production was restored to, in the source's own spelling (004 T140,
	// FR-016). It names an estate — `dpl_7Qk…` is a real Vercel deployment and `checkout-00042-abc`
	// a real Cloud Run revision — so it is pseudonymised rather than kept.
	//
	// The KIND is the decision, and it is `revision` rather than `resource` because of §2.3 property
	// 1. A rollback and the rollout it undoes are joined through this value: the restored
	// deployment's identifier appears here on the rollback AND as the identifier of the change that
	// first shipped it. Property 2 says two kinds of the same name pseudonymise to different tokens,
	// so a mismatch here would give one deployment two tokens, and 004's own acceptance scenario —
	// that the rollback is distinguishable from the rollout it undoes and both appear with their own
	// valid times — would fail in exactly the sanitised corpus that exists to check it. `revision` is
	// the published kind for "one built, deployable version of a service", which is what a Vercel
	// deployment and a Cloud Run revision both are, so whoever adds the row for the deployment
	// identifier itself has one kind to match rather than a choice to make. The test in
	// policy_rollback_test.go pins that.
	//
	// Empty is not a missing disposition. FR-016 makes an empty value with `rollback` true "a
	// rollback whose target we have not been told" rather than "not a rollback", and an empty string
	// pseudonymises to nothing here — the sanitiser is asked about a value only when there is one.
	pseudo("change.rolled_back_to", KindRevision, "the restored deployment or version, in the source's own spelling; joined to the rollout it undoes, so it takes the same kind as every other deployed-version identifier (FR-016, §2.3 property 1)")
	// The deployment production was moved AWAY from (004 T155). Same kind as rolled_back_to and for a
	// sharper version of the same reason: the investigation engine joins this value to the rollout that
	// shipped that deployment and credits the hypothesis naming it, so a token that differed from the
	// rollout's would silently cut the one link that makes a rollback evidence — in the sanitised corpus,
	// where it is tested.
	pseudo("change.rolled_back_from", KindRevision, "the deployment a rollback moved production away from, in the source's own spelling; joined to the rollout that shipped it, so it takes the same kind as every other deployed-version identifier (004 T155, §2.3 property 1)")
	pseudo("notice.vendor_notice_id", KindNoticeID, "the vendor's own notice identifier")
	pseudo("message.messageId", KindNoticeID, "the vendor's own notice identifier, as the transport spells it")

	// --- the label allowlist (config/gcp.yaml) -----------------------------------------------
	//
	// Six keys, and the reasoning is not uniform:

	// FR-135 names environment among the identifiers that MUST be pseudonymised, and it is
	// pseudonymised here even though the *derived* environment enum above is kept verbatim. The
	// two are different facts: `prod-eu-payments` is a string this organisation invented, and
	// `production` is a closed value this project publishes.
	p.labels["environment"] = Rule{Disposition: Pseudonym, Kind: KindEnvironment, Why: "FR-135 names environment explicitly"}
	p.labels["service"] = Rule{Disposition: Pseudonym, Kind: KindService, Why: "an infrastructure identifier"}
	p.labels["component"] = Rule{Disposition: Pseudonym, Kind: KindResource, Why: "an infrastructure identifier"}
	p.labels["team"] = Rule{Disposition: Pseudonym, Kind: KindTeam, Why: "FR-124 builds OWNER nodes from it"}

	// `managed-by` is usually a tool (`terraform`, `gcloud`) and occasionally a team. Nothing in
	// the value distinguishes them, and the contract's asymmetry decides it: a pseudonymised tool
	// name costs one unreadable token in a golden, a recorded team name is a disclosure.
	p.labels["managed-by"] = Rule{Disposition: Pseudonym, Kind: KindTeam, Why: "indistinguishable from a team name, and the asymmetry decides it"}

	// `version` is the one row kept verbatim against the grain of §2.3, and it is a decision
	// rather than an oversight. ADR-0005 D4 makes the deploy version the join between a digest
	// and a change in the graph, and feature 002's default redaction policy leaves
	// `join_keys.version` in the clear. Pseudonymising it here and not there would break that
	// join in exactly the corpus this contract exists to protect. The residual disclosure — a
	// version string that embeds a person's name — is real, and is caught by the independent
	// personal-data scan (FR-138), which is the gate that does not share a defect with this
	// table.
	p.labels["version"] = Rule{Disposition: Verbatim, Why: "ADR-0005 D4's join; the residual name-in-a-version risk is the scan's (FR-138)"}

	return p
}
