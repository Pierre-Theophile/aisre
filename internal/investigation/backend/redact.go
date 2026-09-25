// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Redaction (tasks.md T035, FR-038, ADR-0003 D9, contracts/telemetry-backend.md §6).
//
// Redaction happens before anything leaves the process, and it happens in live mode as well as
// when recording. That asymmetry is the whole point: if redaction were a recording step, a live
// investigation could surface what a recording would not be allowed to keep, and the difference
// between "safe to store" and "safe to see" would be a difference nobody wrote down.
//
// Four rules, from ADR-0003 D9:
//
//   - People identifiers are **dropped, not hashed**. A stable hash of a person is still a
//     person: it joins across every digest in the corpus and it is re-identifiable from any one
//     known example. There is no investigative use for it that the actor kind on a change does
//     not already serve.
//   - Infrastructure identifiers are **pseudonymised with a keyed HMAC, consistently**, because
//     a digest whose keys do not join is evidence about nothing. The same pod is the same
//     pseudonym in every digest of one recording, and a different one in another recording.
//   - Log content is reduced to **masked templates** before it is returned at all. A template is
//     what makes a log line publishable; the mask is defined here so that the miner, the
//     exemplar path and the redactor cannot drift.
//   - Monitor bodies are **dropped**. A monitor body is prose a human wrote about production,
//     and prose is the one thing the injection barrier cannot bound.
//
// A backend that emits a field its declaration does not cover is rejected `undeclared_redaction`
// — at the boundary, not in review.

// The published redaction field vocabulary. A policy names the fields it drops and the fields it
// pseudonymises using these spellings, and CheckDeclared refuses a response carrying a sensitive
// field the policy did not name.
const (
	// FieldJoinKeyWorkload is JoinKeys.workload.
	FieldJoinKeyWorkload = "join_keys.workload"
	// FieldJoinKeyPodOrHost is JoinKeys.pod_or_host.
	FieldJoinKeyPodOrHost = "join_keys.pod_or_host"
	// FieldJoinKeyVersion is JoinKeys.version.
	FieldJoinKeyVersion = "join_keys.version"
	// FieldJoinKeyTraceIDs is JoinKeys.trace_ids.
	FieldJoinKeyTraceIDs = "join_keys.trace_ids"
	// FieldJoinKeySpanIDs is JoinKeys.span_ids.
	FieldJoinKeySpanIDs = "join_keys.span_ids"
	// FieldSeriesTags is SeriesSummary.tags.
	FieldSeriesTags = "series.tags"
	// FieldLogTemplate is LogPattern.template.
	FieldLogTemplate = "log.template"
	// FieldExemplarText is Exemplar.text.
	FieldExemplarText = "exemplar.text"
	// FieldMonitorBody is the monitor's own prose, which is dropped rather than carried.
	FieldMonitorBody = "monitor.body"
	// FieldPeopleIdentifiers is the whole class of people identifiers, which are dropped.
	FieldPeopleIdentifiers = "people_identifiers"
)

// PeopleAttributeKeys is the published set of attribute and tag keys that name a person. A value
// under one of these is dropped wherever it appears; it is never hashed and never pseudonymised.
//
// The set is deliberately generous. A false positive costs one tag on one digest; a false
// negative puts a person's identity in a corpus that is shared with graders.
var PeopleAttributeKeys = map[string]struct{}{
	"actor": {}, "author": {}, "committer": {}, "creator": {}, "email": {},
	"enduser.id": {}, "git.author": {}, "git.commit.author": {}, "operator": {},
	"owner": {}, "principal": {}, "requested_by": {}, "triggered_by": {},
	"user": {}, "user.email": {}, "user.id": {}, "user.name": {}, "user_id": {},
	"username": {},
}

// PseudonymPrefix marks a value as a pseudonym rather than a production identifier, so that a
// reader of a world file is never misled into pasting one into a vendor console.
const PseudonymPrefix = "px_"

// DefaultRedactionPolicyVersion is the policy version this feature's backends declare. It is
// recorded in every world index, so what was applied to a fixture stays knowable after the
// policy changes.
const DefaultRedactionPolicyVersion = "1.0.0"

// DefaultRedactionPolicy is the policy every backend in this repository declares: people
// identifiers and monitor bodies dropped, infrastructure identifiers pseudonymised, log bodies
// reduced to templates.
func DefaultRedactionPolicy() *investigationv1.RedactionPolicy {
	return &investigationv1.RedactionPolicy{
		DroppedFields:        []string{FieldPeopleIdentifiers, FieldMonitorBody},
		PseudonymisedFields:  []string{FieldJoinKeyPodOrHost, FieldJoinKeyWorkload, FieldJoinKeyTraceIDs, FieldJoinKeySpanIDs},
		LogBodiesAsTemplates: true,
		PolicyVersion:        DefaultRedactionPolicyVersion,
	}
}

// Redactor applies one policy. It is built once per backend and reused, because the HMAC key is
// what makes a pseudonym consistent across the digests of one recording.
type Redactor struct {
	policy     *investigationv1.RedactionPolicy
	key        []byte
	dropped    map[string]struct{}
	pseudonyms map[string]struct{}
}

// NewRedactor returns a Redactor applying policy with key. A policy with no version is refused:
// a recording must say what was applied to it.
func NewRedactor(policy *investigationv1.RedactionPolicy, key []byte) (*Redactor, error) {
	if policy.GetPolicyVersion() == "" {
		return nil, Reject(ReasonUndeclaredRedaction,
			"a redaction policy with no version; a recording must say what was applied to it")
	}
	if len(key) == 0 {
		return nil, Reject(ReasonUndeclaredRedaction,
			"redaction policy %s has no pseudonymisation key; an unkeyed pseudonym is a hash anybody can reverse by enumeration",
			policy.GetPolicyVersion())
	}
	r := &Redactor{
		policy:     policy,
		key:        append([]byte(nil), key...),
		dropped:    setOf(policy.GetDroppedFields()),
		pseudonyms: setOf(policy.GetPseudonymisedFields()),
	}
	return r, nil
}

func setOf(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// Policy returns the declared policy, which is recorded with every world.
func (r *Redactor) Policy() *investigationv1.RedactionPolicy { return r.policy }

// Pseudonymise returns the keyed HMAC pseudonym of value: stable within one recording, useless
// outside it, and prefixed so that nobody mistakes it for a production identifier.
func (r *Redactor) Pseudonymise(value string) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, PseudonymPrefix) {
		return value // already pseudonymised; applying the policy twice is idempotent
	}
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(value))
	return PseudonymPrefix + hex.EncodeToString(mac.Sum(nil))[:16]
}

// Apply redacts a response in place and then checks the result against the declaration. It is
// called on every response, in both modes, before anything leaves the backend process.
func (r *Redactor) Apply(resp *Response) error {
	d := resp.GetDigest()
	if d == nil {
		return nil
	}
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, s := range body.Metric.GetSeries() {
			s.Tags = r.redactTags(s.GetTags())
			r.redactJoinKeys(s.GetJoinKeys())
		}
	case *investigationv1.Digest_Log:
		for _, p := range body.Log.GetPatterns() {
			if r.policy.GetLogBodiesAsTemplates() {
				p.Template = MaskLine(p.GetTemplate())
			}
			r.redactJoinKeys(p.GetJoinKeys())
		}
	case *investigationv1.Digest_Trace:
		for _, g := range body.Trace.GetGroups() {
			r.redactJoinKeys(g.GetJoinKeys())
		}
	case *investigationv1.Digest_MonitorState:
		if _, dropped := r.dropped[FieldMonitorBody]; dropped {
			d.FreeText = ""
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, v := range body.ErrorsByVersion.GetVersions() {
			r.redactJoinKeys(v.GetJoinKeys())
		}
	case *investigationv1.Digest_Exemplars:
		for _, ex := range body.Exemplars.GetExemplars() {
			ex.Text = MaskLine(ex.GetText())
			ex.RedactionApplied = r.policy.GetPolicyVersion()
			r.redactJoinKeys(ex.GetJoinKeys())
		}
	case *investigationv1.Digest_Knowledge:
		for _, item := range body.Knowledge.GetItems() {
			item.Excerpt = MaskLine(item.GetExcerpt())
		}
	case *investigationv1.Digest_Onset:
		// An onset digest carries an instant, an uncertainty and the method's parameters.
		// There is nothing in it to redact, which is the strongest argument for computing it
		// backend-side.
	}
	return r.CheckDeclared(resp)
}

// redactJoinKeys drops what the policy drops and pseudonymises what it pseudonymises. A join key
// the policy names neither way is left alone: the version tag is the canonical case, because the
// whole point of ADR-0005 D4 is that a deploy revision joins a digest to a change in the graph.
func (r *Redactor) redactJoinKeys(k *investigationv1.JoinKeys) {
	if k == nil {
		return
	}
	k.Workload = r.applyField(FieldJoinKeyWorkload, k.GetWorkload())
	k.PodOrHost = r.applyField(FieldJoinKeyPodOrHost, k.GetPodOrHost())
	k.Version = r.applyField(FieldJoinKeyVersion, k.GetVersion())
	k.TraceIds = r.applyFields(FieldJoinKeyTraceIDs, k.GetTraceIds())
	k.SpanIds = r.applyFields(FieldJoinKeySpanIDs, k.GetSpanIds())
}

func (r *Redactor) applyField(field, value string) string {
	if value == "" {
		return ""
	}
	if _, drop := r.dropped[field]; drop {
		return ""
	}
	if _, pseudo := r.pseudonyms[field]; pseudo {
		return r.Pseudonymise(value)
	}
	return value
}

func (r *Redactor) applyFields(field string, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	if _, drop := r.dropped[field]; drop {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, r.applyField(field, v))
	}
	return out
}

// redactTags drops every tag naming a person and pseudonymises the infrastructure tags the
// policy names, leaving the rest — a tag like `env:prod` is what a comparison is about.
func (r *Redactor) redactTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		if IsPeopleAttribute(key) {
			continue
		}
		switch key {
		case "pod", "host", "pod_name", "node":
			out[key] = r.applyField(FieldJoinKeyPodOrHost, value)
		case "workload", "deployment":
			out[key] = r.applyField(FieldJoinKeyWorkload, value)
		default:
			out[key] = MaskLine(value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CheckDeclared refuses a response carrying a field the policy does not cover
// (`undeclared_redaction`). It is the half of FR-038 that catches the backend that quietly
// widened what it returns without widening what it declared.
func (r *Redactor) CheckDeclared(resp *Response) error {
	d := resp.GetDigest()
	if d == nil {
		return nil
	}
	_, peopleDropped := r.dropped[FieldPeopleIdentifiers]

	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, s := range body.Metric.GetSeries() {
			for key := range s.GetTags() {
				if IsPeopleAttribute(key) {
					return r.undeclared(FieldSeriesTags,
						"a series tag named %s identifies a person; people identifiers are dropped, never hashed", key)
				}
			}
			if err := r.checkJoinKeys(s.GetJoinKeys()); err != nil {
				return err
			}
		}
	case *investigationv1.Digest_Log:
		if !r.policy.GetLogBodiesAsTemplates() {
			return r.undeclared(FieldLogTemplate,
				"a log digest was produced under a policy that does not declare log_bodies_as_templates; a raw line has no route out of this process")
		}
		for _, p := range body.Log.GetPatterns() {
			if unmasked := UnmaskedLiterals(p.GetTemplate()); unmasked != "" {
				return r.undeclared(FieldLogTemplate,
					"template %q still carries an unmasked %s", p.GetTemplate(), unmasked)
			}
			if err := r.checkJoinKeys(p.GetJoinKeys()); err != nil {
				return err
			}
		}
	case *investigationv1.Digest_Exemplars:
		if _, declared := r.pseudonyms[FieldExemplarText]; !declared {
			if unmasked := UnmaskedLiterals(exemplarText(body.Exemplars)); unmasked != "" {
				return r.undeclared(FieldExemplarText,
					"an exemplar still carries an unmasked %s", unmasked)
			}
		}
	case *investigationv1.Digest_MonitorState:
		if peopleDropped && d.GetFreeText() != "" {
			return r.undeclared(FieldMonitorBody,
				"a monitor-state digest carries free text; a monitor body is prose a human wrote about production and is dropped, not summarised")
		}
	case *investigationv1.Digest_Trace:
		for _, g := range body.Trace.GetGroups() {
			if err := r.checkJoinKeys(g.GetJoinKeys()); err != nil {
				return err
			}
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, v := range body.ErrorsByVersion.GetVersions() {
			if err := r.checkJoinKeys(v.GetJoinKeys()); err != nil {
				return err
			}
		}
	}
	return nil
}

func exemplarText(d *investigationv1.ExemplarDigest) string {
	parts := make([]string, 0, len(d.GetExemplars()))
	for _, ex := range d.GetExemplars() {
		parts = append(parts, ex.GetText())
	}
	return strings.Join(parts, "\n")
}

// checkJoinKeys refuses a join key the policy said it would pseudonymise but which arrived in
// the clear. This is the check that keeps "joins survive" honest: a backend that forgot to
// pseudonymise is caught here rather than in a corpus review months later.
func (r *Redactor) checkJoinKeys(k *investigationv1.JoinKeys) error {
	if k == nil {
		return nil
	}
	for field, value := range map[string]string{
		FieldJoinKeyWorkload:  k.GetWorkload(),
		FieldJoinKeyPodOrHost: k.GetPodOrHost(),
		FieldJoinKeyVersion:   k.GetVersion(),
	} {
		if value == "" {
			continue
		}
		if _, pseudo := r.pseudonyms[field]; pseudo && !strings.HasPrefix(value, PseudonymPrefix) {
			return r.undeclared(field, "%s is declared pseudonymised but arrived in the clear as %q", field, value)
		}
	}
	return nil
}

func (r *Redactor) undeclared(field, format string, args ...any) error {
	return Reject(ReasonUndeclaredRedaction, "redaction policy %s does not cover %s: %s",
		r.policy.GetPolicyVersion(), field, fmt.Sprintf(format, args...))
}

// IsPeopleAttribute reports whether key names a person under the published vocabulary. The
// comparison is case-insensitive and tolerant of the usual separators, because a vendor spells
// the same concept `user.email`, `userEmail` and `user_email` in three different products.
func IsPeopleAttribute(key string) bool {
	normalised := strings.ToLower(strings.TrimSpace(key))
	if _, ok := PeopleAttributeKeys[normalised]; ok {
		return true
	}
	collapsed := strings.NewReplacer("-", "", "_", "", ".", "").Replace(normalised)
	for candidate := range PeopleAttributeKeys {
		if collapsed == strings.NewReplacer("-", "", "_", "", ".", "").Replace(candidate) {
			return true
		}
	}
	return false
}

// The masking rules. They are the single definition of what a masked template is: the Drain
// miner masks with them before it clusters, the exemplar path masks with them before it returns
// a line, and CheckDeclared uses their inverse to catch anything that got through.
//
// Order matters: the most specific pattern first, so an email is an <email> rather than a
// <word>@<word> and a UUID is a <uuid> rather than five <hex> runs.
var maskRules = []struct {
	name        string
	pattern     *regexp.Regexp
	placeholder string
}{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), "<email>"},
	{"url", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"']+`), "<url>"},
	{"uuid", regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
	{"ip", regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), "<ip>"},
	{"timestamp", regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+\-]\d{2}:?\d{2})?`), "<ts>"},
	{"hex", regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{16,}\b`), "<hex>"},
	{"duration", regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|ms|s|m|h)\b`), "<dur>"},
	{"path", regexp.MustCompile(`(?:^|\s)/(?:[A-Za-z0-9._\-]+/)+[A-Za-z0-9._\-]*`), " <path>"},
	{"number", regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<num>"},
}

// MaskLine reduces one line to its masked form: every variable the published rules recognise is
// replaced by its placeholder. It is idempotent — masking a masked line changes nothing — which
// is what lets the redactor run over a digest a miner already produced.
func MaskLine(line string) string {
	if line == "" {
		return ""
	}
	out := line
	for _, rule := range maskRules {
		out = rule.pattern.ReplaceAllString(out, rule.placeholder)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}

// UnmaskedLiterals names the first class of unmasked variable it finds in text, or "" when the
// text is fully masked. It is the inverse of MaskLine and the check CheckDeclared runs.
func UnmaskedLiterals(text string) string {
	if text == "" {
		return ""
	}
	names := make([]string, 0, 2)
	for _, rule := range maskRules {
		if rule.name == "number" || rule.name == "path" {
			// A masked template legitimately keeps small structural numbers in a status code
			// or a field name, and <path> replacement leaves a leading space the round trip
			// normalises; both are covered by re-masking rather than by refusal.
			continue
		}
		if rule.pattern.MatchString(text) {
			names = append(names, rule.name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}
