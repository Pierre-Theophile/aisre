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

// LogSource is one watched log source: a Datadog service in one environment.
type LogSource struct {
	// Env is the Datadog `env` the source's lines carry. Required: a service name is unique only
	// within one environment, and C9 requires it.
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
	case strings.TrimSpace(s.Env) == "":
		return fmt.Errorf("datadog: log source %q states no environment; a service name is unique only "+
			"within one, and the rule that merges it (C9) never fires without one", s.Service)
	case strings.TrimSpace(s.Service) == "":
		return fmt.Errorf("datadog: a log source in %q names no service", s.Env)
	case strings.Contains(s.Env, "/") || strings.Contains(s.Service, "/"):
		return fmt.Errorf("datadog: log source %s/%s contains a `/`, which the ref value uses as its "+
			"separator", s.Env, s.Service)
	}
	return nil
}

// Ref addresses the source's service node: `datadog.service=<env>/<service>`.
func (s LogSource) Ref() *graphv1.Ref { return feeder.Ref(NSService, s.Env+"/"+s.Service) }

// ParseLogSource reads `<env>/<service>`, the spelling of `--watch`.
func ParseLogSource(spec string) (LogSource, error) {
	env, service, ok := strings.Cut(strings.TrimSpace(spec), "/")
	src := LogSource{Env: env, Service: service}
	if !ok {
		return src, fmt.Errorf("datadog: log source %q is not `<env>/<service>`", spec)
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
	props, err := feeder.NewProps().
		Str(feeder.AttrServiceName, src.Service).
		Str(feeder.AttrDeploymentEnvironment, src.Env).
		Build()
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
