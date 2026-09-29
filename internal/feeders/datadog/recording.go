// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// Recording a live run without writing an unsanitised byte (T080; FR-076a, FR-137).
//
// The recording goes through internal/feeders/deployrecord's tee: the live feeder gets Datadog's bytes,
// and the recording gets only the sanitiser's output. Datadog needs a pre-pass before the policy's
// field table, because its identifiers also live inside free text — `service:checkout` in a monitor's
// query, `env:production,service:checkout` as a group key, `team:payments` as a tag, `production/checkout`
// as a watched source — and each must become the SAME pseudonym everywhere, or the recording's monitors
// would watch services its discovery never names. The pre-pass rewrites every `key:value` term whose key
// names an identifier with the sanitiser's keyed pseudonym for that kind; the field table then decides
// every field, and an unknown one fails closed.
//
// What the pre-pass cannot recognise it cannot rewrite, which is why the query is a field the table
// keeps only after this pass, and why the recording is still scanned for canaries before a byte lands.

// termKinds are the query and tag keys whose values are identifiers, and the kind each is keyed as.
var termKinds = map[string]sanitise.Kind{
	"service":         sanitise.KindService,
	"env":             sanitise.KindEnvironment,
	"host":            sanitise.KindHost,
	"team":            sanitise.KindTeam,
	"owner":           sanitise.KindTeam,
	"kube_namespace":  sanitise.KindNamespace,
	"kube_deployment": sanitise.KindService,
	"kube_cluster":    sanitise.KindCluster,
}

var term = regexp.MustCompile(`(^|[\s{,("'!])(service|env|host|team|owner|kube_namespace|kube_deployment|kube_cluster):([A-Za-z0-9_.\-/]+)`)

// pseudonymiseTerms rewrites every identifier term in free text.
func pseudonymiseTerms(san *sanitise.Sanitiser, text string) (string, error) {
	var failure error
	out := term.ReplaceAllStringFunc(text, func(match string) string {
		m := term.FindStringSubmatch(match)
		value, err := san.Identifier(termKinds[m[2]], m[3])
		if err != nil {
			failure = err
			return match
		}
		return m[1] + m[2] + ":" + value
	})
	return out, failure
}

// recordedQuery is what a recording keeps of a monitor's query: its identifier terms, pseudonymised,
// and nothing else. The rest of a query is free text — a metric name that embeds a service, a search
// phrase — which no pre-pass can vouch for. A replay reads only these terms (the watched service and
// environment), and the query pointer executes against the monitor's id, so nothing it needs is lost.
func recordedQuery(san *sanitise.Sanitiser, query string) (string, error) {
	var terms []string
	for _, m := range term.FindAllStringSubmatch(query, -1) {
		value, err := san.Identifier(termKinds[m[2]], m[3])
		if err != nil {
			return "", err
		}
		terms = append(terms, m[2]+":"+value)
	}
	return "recorded: " + strings.Join(terms, " "), nil
}

// pseudonymousSource maps `<env>/<service>` into the recording's vocabulary.
func pseudonymousSource(san *sanitise.Sanitiser, spec string) (string, error) {
	env, service, ok := strings.Cut(spec, "/")
	if !ok {
		// An environment-less source: its service alone.
		return san.Identifier(sanitise.KindService, spec)
	}
	e, err := san.Identifier(sanitise.KindEnvironment, env)
	if err != nil {
		return "", err
	}
	s, err := san.Identifier(sanitise.KindService, service)
	if err != nil {
		return "", err
	}
	return e + "/" + s, nil
}

// PreparePayload is the pre-pass for deployrecord.Tee.WithPrepare.
func PreparePayload(san *sanitise.Sanitiser) func(kind string, raw []byte) ([]byte, error) {
	return func(kind string, raw []byte) ([]byte, error) {
		switch kind {
		case PayloadMonitors:
			return prepareMonitors(san, raw)
		case PayloadDiscovery:
			return prepareDiscovery(san, raw)
		case PayloadPoll:
			return raw, nil
		default:
			return nil, fmt.Errorf("datadog: no pre-pass for payload kind %q", kind)
		}
	}
}

// prepareMonitors keeps only the fields the feeder reads — everything else a monitor carries (its
// creator, options, escalation text, roles, downtimes, organisation) never reaches the recording, so a
// field Datadog adds tomorrow cannot either — and pseudonymises the identifiers inside the query, the
// tags and the group keys. A tag whose key names no known identifier is dropped: it may carry anything.
func prepareMonitors(san *sanitise.Sanitiser, raw []byte) ([]byte, error) {
	var page []monitorJSON
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	for i := range page {
		m := &page[i]
		query, err := recordedQuery(san, m.Query)
		if err != nil {
			return nil, err
		}
		m.Query = query
		var tags []string
		for _, tag := range m.Tags {
			key, _, _ := strings.Cut(tag, ":")
			if termKinds[key] == "" {
				continue
			}
			clean, err := pseudonymiseTerms(san, tag)
			if err != nil {
				return nil, err
			}
			tags = append(tags, clean)
		}
		m.Tags = tags
		groups := make(map[string]groupJSON, len(m.State.Groups))
		for key, g := range m.State.Groups {
			clean, err := pseudonymiseTerms(san, key)
			if err != nil {
				return nil, err
			}
			groups[clean] = g
		}
		m.State.Groups = groups
	}
	return json.Marshal(page)
}

func prepareDiscovery(san *sanitise.Sanitiser, raw []byte) ([]byte, error) {
	var tick DiscoveryTick
	if err := json.Unmarshal(raw, &tick); err != nil {
		return nil, err
	}
	var err error
	for i, s := range tick.LogSources {
		if tick.LogSources[i], err = pseudonymousSource(san, s); err != nil {
			return nil, err
		}
	}
	for i := range tick.Measurements {
		m := &tick.Measurements[i]
		if m.Source, err = pseudonymousSource(san, m.Source); err != nil {
			return nil, err
		}
		m.Failed, m.ValuesFailed = redactReason(m.Failed), redactReason(m.ValuesFailed)
		if m.EnvDiscovery != nil {
			for j := range m.EnvDiscovery.Values {
				if m.EnvDiscovery.Values[j].Value, err = san.Identifier(sanitise.KindEnvironment, m.EnvDiscovery.Values[j].Value); err != nil {
					return nil, err
				}
			}
		}
		for j := range m.Tags {
			t := &m.Tags[j]
			switch {
			case t.Key == KubeDeploymentPair:
				ns, dep, _ := strings.Cut(t.Value, "/")
				a, err := san.Identifier(sanitise.KindNamespace, ns)
				if err != nil {
					return nil, err
				}
				b, err := san.Identifier(sanitise.KindService, dep)
				if err != nil {
					return nil, err
				}
				t.Value = a + "/" + b
			case termKinds[t.Key] != "":
				if t.Value, err = san.Identifier(termKinds[t.Key], t.Value); err != nil {
					return nil, err
				}
			default:
				// A key the poller should never have measured: its value is not written.
				t.Value = ""
			}
		}
	}
	return json.Marshal(tick)
}

// redactReason keeps that a read failed, never the vendor's words, which quote URLs and names. A typed
// stop is the connector's own word and is kept.
func redactReason(reason string) string {
	if reason == "" || reason == StopQuota || reason == StopRateLimited {
		return reason
	}
	return "the read failed (reason withheld from the recording)"
}

// PseudonymousOptions maps a live run's options into the recording's vocabulary, for the shadow feeder
// that derives the recording's events from its sanitised payloads.
func PseudonymousOptions(san *sanitise.Sanitiser, opts Options) (Options, error) {
	out := opts
	out.MonitorTags = nil
	for _, tag := range opts.MonitorTags {
		clean, err := pseudonymiseTerms(san, tag)
		if err != nil {
			return Options{}, err
		}
		out.MonitorTags = append(out.MonitorTags, clean)
	}
	out.LogSources = nil
	for _, src := range opts.LogSources {
		spec, err := pseudonymousSource(san, src.Key())
		if err != nil {
			return Options{}, err
		}
		clean, _ := ParseLogSource(spec)
		clean.Index, clean.VersionAttribute = src.Index, src.VersionAttribute
		out.LogSources = append(out.LogSources, clean)
	}
	out.VersionOverrides = map[string]string{}
	for key, v := range opts.VersionOverrides {
		spec, err := pseudonymousSource(san, key)
		if err != nil {
			return Options{}, err
		}
		out.VersionOverrides[spec] = v
	}
	out.Log = nil
	return out, nil
}
