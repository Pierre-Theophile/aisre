// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// A watched log source is a service node, whatever deployed the service (005 FR-040f, T063).
//
// The connector asserts a SERVICE node for every configured log source, every time, and never asks the
// graph whether another feeder has reported the same service first. What a connector emits must not
// depend on what other sources happened to deliver before it (constitution III): an emission conditional
// on graph state would build a different graph for each delivery order. Where Cloud Run, Kubernetes or
// OpenTelemetry also reports the service, the `datadog.log_service` claim below is what C9 merges on,
// in the same environment only; where nothing else reports it — a service on a vendor-hosted runtime, a
// VM, a platform no feeder covers — this node is the one an investigation starts from.

// LogSource is one watched log source: a Datadog service, in one environment when its lines carry one.
type LogSource struct {
	// Env is the Datadog `env` the source's lines carry. Empty when they carry none (005): many
	// organisations ship logs without unified service tagging's `env`, and an agent installed there must
	// still measure them. Such a source is stated as environment-less everywhere it appears, and it is
	// never merged by C9, which needs the environment: a service name is unique only within one.
	Env string
	// Service is the Datadog `service` the source's lines carry. Required.
	Service string
	// Index is the log index to search, where one is configured. Empty searches the default.
	Index string
	// VersionAttribute overrides version-stamp discovery with one named attribute, spelled as the
	// selector grammar spells it (`version` for the tag, `@version` for the attribute).
	VersionAttribute string
}

// Validate refuses a source that could not be addressed or merged correctly.
func (s LogSource) Validate() error {
	switch {
	case strings.TrimSpace(s.Service) == "":
		return fmt.Errorf("datadog: a log source in %q names no service", s.Env)
	case strings.Contains(s.Env, "/") || strings.Contains(s.Service, "/"):
		return fmt.Errorf("datadog: log source %s/%s contains a `/`, which the ref value uses as its "+
			"separator", s.Env, s.Service)
	}
	return nil
}

// Key is the source's spelling: `<env>/<service>`, or `<service>` for an environment-less source.
func (s LogSource) Key() string {
	if s.Env == "" {
		return s.Service
	}
	return s.Env + "/" + s.Service
}

// Ref addresses the source's service node: `datadog.service=<env>/<service>`, or
// `datadog.service=<service>` when its lines carry no environment.
func (s LogSource) Ref() *graphv1.Ref { return feeder.Ref(NSService, s.Key()) }

// Query is the source's Datadog search: its service, and its environment when it has one.
func (s LogSource) Query() string {
	if s.Env == "" {
		return "service:" + s.Service
	}
	return "service:" + s.Service + " env:" + s.Env
}

// MissingEnvWarning is what the connector says, wherever a source is configured or measured, about a
// service whose logs carry no environment.
func MissingEnvWarning(service string) string {
	return fmt.Sprintf("missing env to match service %q: its logs carry no `env` tag, so it cannot be matched "+
		"to the same service on other platforms (C9 needs the environment). Please add the environment to "+
		"these logs: DD_ENV=<env> on the service, the tags.datadoghq.com/env label on Kubernetes, or an "+
		"`env:<env>` tag on the log pipeline (docs/connectors/version-stamping.md)", service)
}

// ParseLogSource reads the spelling of `--watch`: `<env>/<service>`, or `<service>` for a service
// whose logs carry no environment.
func ParseLogSource(spec string) (LogSource, error) {
	spec = strings.TrimSpace(spec)
	env, service, ok := strings.Cut(spec, "/")
	src := LogSource{Env: env, Service: service}
	if !ok {
		src = LogSource{Service: spec}
	}
	return src, src.Validate()
}

// LogSourceEvents returns the events that assert a watched source's service node and its C9 claim, as
// observed at `at`. The node's valid start is unknown: Datadog states when a service's lines were
// first indexed, not when the service began (FR-017).
//
// The ids are a pure function of the source, so asserting an unchanged source again re-sends ids
// already sent and is a no-op. When the node gains pointers (T065) the pointer state joins the id, so a
// changed verdict is a new assertion rather than a DUPLICATE_NOOP — 003's T066/T183 lesson.
func LogSourceEvents(desc feeder.Description, src LogSource, at time.Time) ([]*graphv1.EventEnvelope, error) {
	if err := src.Validate(); err != nil {
		return nil, err
	}
	subject := src.Ref()
	builder := feeder.NewProps().Str(feeder.AttrServiceName, src.Service)
	if src.Env != "" {
		builder.Str(feeder.AttrDeploymentEnvironment, src.Env)
	}
	props, err := builder.Build()
	if err != nil {
		return nil, err
	}
	node := feeder.UpsertNode(desc, feeder.NewID(desc.SourceID, "service", subject.GetValue()), feeder.NodeFact{
		Meta:             feeder.Meta{SourceObservedAt: at},
		Ref:              subject,
		Type:             graphv1.NodeType_SERVICE,
		DisplayName:      src.Service,
		Props:            props,
		ValidFromUnknown: true,
	})

	// The service name is a CORRELATION key, not an identity claim. The same name is carried by this
	// source's production and staging nodes, and by another organisation's; an identity claim is
	// unique per (namespace, value, source), so a shared name stored as one would land on whichever
	// node the projector processed first. datadog-log-service-merge-01's shuffle step caught exactly
	// that, as feature 004 found for deploy.* (pkg/feeder/claim.go). C9 reads the key, and corroborates
	// it with the environment below: a correlation alone never merges anything.
	if src.Env == "" {
		// No environment, no C9 claim: the name alone would merge this organisation's services across
		// every environment they run in. The node stands on its own, and the checkpoint says why.
		return []*graphv1.EventEnvelope{node}, nil
	}
	attrs, err := feeder.NewProps().Str(AttrEnvironment, src.Env).Build()
	if err != nil {
		return nil, err
	}
	claim := feeder.Correlate(desc,
		feeder.NewID(desc.SourceID, "correlation", subject.GetValue(), NSLogService, src.Service),
		feeder.CorrelationFact{
			Meta:       feeder.Meta{SourceObservedAt: at},
			Subject:    subject,
			Key:        feeder.Ref(NSLogService, src.Service),
			Attributes: attrs,
		})
	return []*graphv1.EventEnvelope{node, claim}, nil
}
