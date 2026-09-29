// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// Sanitisation of a live answer (FR-052): the Datadog table first, then feature 002's redactor.
//
// The order is the same as the GCP backend's, for the same reason. The join keys are pseudonymised
// with the corpus key and the table the feeder used, so a digest's `host` token is the token the graph
// carries; 002's redactor then passes a `px_` value through untouched. The deployed version is the one
// join key left verbatim, and the table says why: it is what joins a digest to the deploy feeders'
// changes, and it is not a people or infrastructure identifier (sanitise.DatadogPathVersion).

// Sanitiser is the Datadog sanitisation contract as the backend applies it.
type Sanitiser interface {
	Policy() *sanitise.Policy
	Template(where, line string) (string, bool, error)
	Field(where, path, value string) (string, bool, error)
	AssertArtifact(where string, data []byte) error
}

// DeclaredRedactionPolicy is the policy this backend publishes, derived from the table rather than
// written out beside it.
func DeclaredRedactionPolicy() *investigationv1.RedactionPolicy {
	policy := sanitise.DatadogPolicy()
	return &investigationv1.RedactionPolicy{
		DroppedFields: append([]string{engine.FieldPeopleIdentifiers, engine.FieldMonitorBody},
			policy.Fields(sanitise.Dropped)...),
		PseudonymisedFields: append([]string{engine.FieldJoinKeyWorkload, engine.FieldJoinKeyPodOrHost},
			policy.Fields(sanitise.Pseudonym)...),
		LogBodiesAsTemplates: true,
		PolicyVersion:        policy.Version(),
	}
}

// SanitiseThenRedact applies the table to a live answer, then 002's redactor.
func SanitiseThenRedact(s Sanitiser, r *engine.Redactor, resp *engine.Response) error {
	if s == nil || r == nil {
		return fmt.Errorf("datadog: a live answer with no sanitiser or no redactor; FR-052 says a live " +
			"investigation must not surface what a recording would not be allowed to keep")
	}
	if log := resp.GetDigest().GetLog(); log != nil {
		kept := log.GetPatterns()[:0]
		for _, pattern := range log.GetPatterns() {
			template, keep, err := s.Template("a live log digest", pattern.GetTemplate())
			if err != nil {
				return err
			}
			if keep {
				pattern.Template = template
				kept = append(kept, pattern)
			}
		}
		log.Patterns = kept
	}
	if trace := resp.GetDigest().GetTrace(); trace != nil {
		// An operation is a span's resource name, which routinely carries a route with an identifier or a
		// statement with its literals: it goes through the same masking a log line does, and a group whose
		// name cannot be kept safe is named as withheld rather than dropped, so its count still counts.
		for _, group := range trace.GetGroups() {
			for _, field := range []*string{&group.Operation, &group.ErrorKind} {
				if *field == "" {
					continue
				}
				template, keep, err := s.Template("a live span digest", *field)
				if err != nil {
					return err
				}
				if keep {
					*field = template
				} else {
					*field = "(withheld by the sanitiser)"
				}
			}
		}
	}
	for _, keys := range joinKeysIn(resp.GetDigest()) {
		if err := sanitiseJoinKeys(s, keys); err != nil {
			return err
		}
	}
	if monitor := resp.GetDigest().GetMonitorState(); monitor != nil {
		// Group keys name the resources a monitor alerts on, in the transitions and in the per-group
		// map alike: one token for one group, so the two stay joinable.
		group := func(key string) (string, error) {
			if key == "" {
				return "", nil
			}
			token, keep, err := s.Field("a monitor-state group", sanitise.DatadogPathMonitorGroup, key)
			if err != nil || !keep {
				return "", err
			}
			return token, nil
		}
		for _, transition := range monitor.GetTransitions() {
			token, err := group(transition.GetGroupKey())
			if err != nil {
				return err
			}
			transition.GroupKey = token
		}
		if states := monitor.GetPerGroupState(); len(states) > 0 {
			out := make(map[string]string, len(states))
			for key, state := range states {
				token, err := group(key)
				if err != nil {
					return err
				}
				if token != "" {
					out[token] = state
				}
			}
			monitor.PerGroupState = out
		}
	}
	return r.Apply(resp)
}

func joinKeysIn(d *investigationv1.Digest) []*investigationv1.JoinKeys {
	var out []*investigationv1.JoinKeys
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Log:
		for _, p := range body.Log.GetPatterns() {
			out = append(out, p.GetJoinKeys())
		}
	case *investigationv1.Digest_Metric:
		for _, s := range body.Metric.GetSeries() {
			out = append(out, s.GetJoinKeys())
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, v := range body.ErrorsByVersion.GetVersions() {
			out = append(out, v.GetJoinKeys())
		}
	case *investigationv1.Digest_Trace:
		for _, g := range body.Trace.GetGroups() {
			out = append(out, g.GetJoinKeys())
		}
	case *investigationv1.Digest_Exemplars:
		for _, e := range body.Exemplars.GetExemplars() {
			out = append(out, e.GetJoinKeys())
		}
	}
	return out
}

func sanitiseJoinKeys(s Sanitiser, keys *investigationv1.JoinKeys) error {
	if keys == nil {
		return nil
	}
	for _, field := range []struct {
		path  string
		value *string
	}{
		{sanitise.DatadogPathVersion, &keys.Version},
		{sanitise.DatadogPathService, &keys.Workload},
		{sanitise.DatadogPathHost, &keys.PodOrHost},
	} {
		if strings.TrimSpace(*field.value) == "" {
			continue
		}
		token, keep, err := s.Field("a digest join key", field.path, *field.value)
		if err != nil {
			return err
		}
		if !keep {
			token = ""
		}
		*field.value = token
	}
	return nil
}
