// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The telemetry backend a deployment that has configured none gets.
//
// A server started with `--enable-investigation` and no recorded world can still resolve a
// subject, rank the graph's candidates and publish the provisional answer — those are graph
// reads. What it cannot do is test anything, and there are two ways to express that.
//
// The wrong one is to register no telemetry workers: the first wave then asks for a capability
// nobody declares, the engine returns an error, and the run ends `failed`. That throws away the
// provisional ranking, reports a configuration gap as an engine defect, and gives the on-call a
// message about worker routing when the actual problem is a missing flag.
//
// The right one is this: a backend that declares every telemetry term and answers each with
// `query_failed / not_permitted`, naming what is missing. Each call becomes an evidence item with
// a reason, each hypothesis it would have tested stays untested rather than refuted, and the
// investigation concludes with a ranked-but-untested answer and "configure a telemetry recording"
// visible in the record. That is FR-027's whole discipline — a failure is an answer with a reason,
// never a silence — applied to the configuration itself.

// UnconfiguredBackendName is the published name of the backend below. It appears in a
// `worker_calls` row's `backend` column, so an operator reading the rows can see at a glance that
// the run had nothing to ask.
const UnconfiguredBackendName = "unconfigured"

// unconfiguredTelemetry is a TelemetryBackend that answers every term with the same refusal.
type unconfiguredTelemetry struct {
	detail string
}

var _ sdk.TelemetryBackend = (*unconfiguredTelemetry)(nil)

// UnconfiguredTelemetry returns the backend a deployment with no telemetry recording gets.
//
// `detail` is what the operator should do about it, and it travels into every evidence item the
// run produces: the record says what was missing, not merely that something was.
func UnconfiguredTelemetry(detail string) sdk.TelemetryBackend {
	if detail == "" {
		detail = "no telemetry backend is configured on this server"
	}
	return &unconfiguredTelemetry{detail: detail}
}

// Describe declares every telemetry term, priced exactly as a real backend prices it.
//
// Declaring them is the point: the engine routes by declared capability, and a backend that
// declared nothing would be a backend the router cannot reach — which is the failure this type
// exists to avoid. Pricing them normally means a budget spent here is the budget that would have
// been spent for real, so a run against an unconfigured deployment stops where a configured one
// would.
func (b *unconfiguredTelemetry) Describe() sdk.Description {
	terms := sdk.Terms(sdk.FamilyTelemetry)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := backend.PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{Name: term, ReadOnly: true, CostClass: class})
	}
	return sdk.Description{
		Name:           UnconfiguredBackendName,
		Vendor:         UnconfiguredBackendName,
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Version:        sdk.SDKVersion,
		AlgebraVersion: backend.AlgebraVersion,
	}
}

// Execute refuses, with the reason.
//
// The outcome is `query_failed / not_permitted` and never `no_data`. Only `no_data` means
// "nothing happened in production", and a deployment that cannot look has learned nothing about
// production at all (FR-027, Invariant 8).
func (b *unconfiguredTelemetry) Execute(ctx context.Context, req *sdk.AlgebraRequest) (*sdk.AlgebraResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := backend.TermName(req.GetTerm())
	if name == "" || backend.FamilyOf(name) != sdk.FamilyTelemetry {
		return backend.NewResponse(backend.ResponseInput{
			Request: req,
			Outcome: backend.QueryFailed{
				Reason: investigationv1.FailureReason_OUTSIDE_ALGEBRA,
				Detail: "term " + name + " is not in the telemetry family",
			},
			Mode:           backend.ModeLive,
			BackendVersion: sdk.SDKVersion,
		})
	}
	return backend.NewResponse(backend.ResponseInput{
		Request: req,
		Outcome: backend.QueryFailed{
			Reason: investigationv1.FailureReason_NOT_PERMITTED,
			Detail: b.detail,
		},
		Mode:           backend.ModeLive,
		CostClass:      backend.PublishedCostClass(name),
		BackendVersion: sdk.SDKVersion,
	})
}
