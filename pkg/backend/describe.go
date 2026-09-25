// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Describing a backend (tasks.md T036, contracts/cli.md §Workers and backends).
//
// A declaration has to be readable three ways: as a Go value a backend author writes, as the
// published protobuf message an RPC returns, and as the table `backend list` prints. This file
// is the one place the three are related, so that what an operator reads and what the budget
// manager spends against are the same statement.

// Proto renders the declaration as the published BackendDescription message.
func (d Description) Proto() *investigationv1.BackendDescription {
	terms := append([]string(nil), d.Terms...)
	sort.Strings(terms)
	costs := make(map[string]CostClass, len(d.CostClasses))
	for term, class := range d.CostClasses {
		costs[term] = class
	}
	return &investigationv1.BackendDescription{
		Name:           d.Name,
		Vendor:         d.Vendor,
		Terms:          terms,
		CostClasses:    costs,
		Redaction:      d.Redaction,
		Version:        d.Version,
		AlgebraVersion: d.AlgebraVersion,
	}
}

// DescriptionFromProto is the inverse, for a backend served over the wire by
// TelemetryBackendService.Describe.
func DescriptionFromProto(msg *investigationv1.BackendDescription) Description {
	d := Description{
		Name:           msg.GetName(),
		Vendor:         msg.GetVendor(),
		Terms:          append([]string(nil), msg.GetTerms()...),
		CostClasses:    make(map[string]CostClass, len(msg.GetCostClasses())),
		Redaction:      msg.GetRedaction(),
		Version:        msg.GetVersion(),
		AlgebraVersion: msg.GetAlgebraVersion(),
	}
	for term, class := range msg.GetCostClasses() {
		d.CostClasses[term] = class
	}
	sort.Strings(d.Terms)
	return d
}

// TermRow is one line of `backend list`: a term, its cost class, and the widest window the
// backend will answer it over.
type TermRow struct {
	// Term is the published term name.
	Term string `json:"term"`
	// CostClass is the published class the budget manager spends against.
	CostClass string `json:"cost_class"`
	// MaxWindowSeconds is zero when the backend declares no limit of its own.
	MaxWindowSeconds int64 `json:"max_window_seconds,omitempty"`
	// ReadOnly is always true for a registered backend; it is printed anyway, because the
	// point of the declaration is that a reader does not have to take it on trust.
	ReadOnly bool `json:"read_only"`
}

// Rows renders the declaration as the sorted table `backend list` prints.
func (d Description) Rows() []TermRow {
	rows := make([]TermRow, 0, len(d.Terms))
	for _, term := range d.Terms {
		row := TermRow{
			Term:      term,
			CostClass: CostClassName(d.CostClasses[term]),
			ReadOnly:  true,
		}
		if c, ok := d.Capability(term); ok {
			row.MaxWindowSeconds = int64(c.MaxWindow.Seconds())
			row.ReadOnly = c.ReadOnly
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Term < rows[j].Term })
	return rows
}

// CostClassName renders a cost class in the published lowercase spelling — the spelling a
// declaration, a budget profile and `backend list` all use.
func CostClassName(class CostClass) string {
	switch class {
	case CostClassCheap:
		return "cheap"
	case CostClassStandard:
		return "standard"
	case CostClassExpensive:
		return "expensive"
	default:
		return "unspecified"
	}
}

// ParseCostClass reads the published lowercase spelling back. Anything else is rejected rather
// than mapped to the unspecified zero value, because the set is closed.
func ParseCostClass(name string) (CostClass, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cheap":
		return CostClassCheap, nil
	case "standard":
		return CostClassStandard, nil
	case "expensive":
		return CostClassExpensive, nil
	default:
		return CostClassUnspecified, Reject(ReasonWriteCapability,
			"%q is not a published cost class; the closed set is cheap, standard, expensive", name)
	}
}

// QuotaReporting says whether this backend's coverage blocks carry the vendor's remaining
// quota. `backend list` prints it because the budget manager can only spend a share of a quota
// a backend actually reports.
func (d Description) QuotaReporting() string {
	switch d.Vendor {
	case "recorded", "synthetic":
		return "none (answers from a recording; there is no vendor quota to spend)"
	case "gcp":
		// Not a gap in the implementation: GCP publishes no in-band remaining figure on any of
		// the APIs this backend reads. There are no rate-limit response headers, and the first
		// signal that a quota is gone is HTTP 429. The only vendor-reported usage figure is
		// Cloud Monitoring's `serviceruntime` pair, from which remaining must be COMPUTED — a
		// minutes-stale reconciliation signal that itself spends monitoring.query quota. So
		// every coverage block sets quota_undetermined, and this row says why rather than
		// promising a number the budget manager would then wait for.
		return "none in band (GCP reports no remaining figure; every coverage block sets quota_undetermined)"
	default:
		return "as the vendor reports it, in Coverage.remaining_quota"
	}
}
