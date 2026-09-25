// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The spend report (T072, FR-044, FR-048, SC-008).
//
// Consumption is reported against **every** budget, in machine-readable form, including calls per
// worker and elapsed time. The primary unit is tokens per model id per class plus worker calls
// per backend per cost class — provider-independent, and therefore comparable across a year in
// which the vendor changed its prices twice. The monetary figure is derived from the versioned
// price table, and the table's version is stored on the investigation so a cost printed a year
// ago still says what it meant.
//
// It is not a summary. A spend report that only said "$0.42" would be unusable for the two
// questions operators actually ask — "which backend is this costing me" and "is the cache
// working" — and both are answerable only from the per-class, per-backend breakdown.

// Spend renders the machine-readable consumption report.
//
// The client is what prices it: the price table lives with the model configuration, and a run
// that cannot price itself reports its tokens and says so rather than inventing a zero.
func (m *Manager) Spend(client Client) (*investigationv1.BudgetSpend, error) {
	m.mu.Lock()
	tokens := m.tokens
	elapsed := m.elapsedLocked()
	widest := m.widestWindowSecs
	entered := m.reserveEntered
	callsByBackend := make(map[string]int64, len(m.callsByBackend))
	for k, v := range m.callsByBackend {
		callsByBackend[k] = v
	}
	callsByWorker := make(map[string]int64, len(m.callsByWorker))
	for k, v := range m.callsByWorker {
		callsByWorker[k] = v
	}
	callsByClass := make(map[string]int64, len(m.callsByCostClass))
	for k, v := range m.callsByCostClass {
		callsByClass[k] = v
	}
	m.mu.Unlock()

	out := &investigationv1.BudgetSpend{
		Limits:                 m.profile.Proto(),
		WallTimeSeconds:        int64(elapsed / time.Second),
		TokensByModelAndClass:  tokens.Flat(),
		CallsByWorker:          callsByWorker,
		CallsByBackend:         callsByBackend,
		CallsByCostClass:       callsByClass,
		QuotaShareUsed:         m.QuotaShareUsed(),
		RemainingQuotaObserved: m.RemainingQuotaObserved(),
		WidestWindowSeconds:    widest,
	}
	if !entered.IsZero() {
		out.ReserveEnteredAt = timestamppb.New(entered)
	}
	if client != nil {
		cost, err := client.PriceTokens(tokens)
		if err != nil {
			return nil, err
		}
		out.CostUnits = cost
		out.PriceTableVersion = client.PriceTableVersion()
	}
	return out, nil
}

// Client is the narrow view of the model client the spend report needs: a price table with a
// version. It is an interface rather than the concrete client so that the budget package does not
// have to be handed a live API client to render a report.
type Client interface {
	// PriceTokens turns a token ledger into a monetary figure in the table's currency.
	PriceTokens(tokens *model.TokenLedger) (float64, error)
	// PriceTableVersion is the version stored on the investigation.
	PriceTableVersion() string
}

// Report renders the spend for a person, one line per budget, in a fixed order.
//
// This is what `investigate show --spend` prints and what a fixture asserts on, so the ordering is
// deterministic and the numbers are the same ones the machine-readable form carries.
func (m *Manager) Report() string {
	var b strings.Builder
	profile := m.Profile()
	fmt.Fprintf(&b, "budget profile: %s\n", profile.Describe())
	fmt.Fprintf(&b, "mode: %s", m.Mode())
	if reason := m.ReserveReason(); reason != "" {
		fmt.Fprintf(&b, " (%s)", reason)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "elapsed: %s\n", m.Elapsed().Round(time.Millisecond))
	if d := m.FirstTestedAfter(); d > 0 {
		fmt.Fprintf(&b, "first tested hypothesis after: %s (target %s)\n", d.Round(time.Millisecond), profile.FirstTestedTarget)
	} else {
		b.WriteString("first tested hypothesis after: not reached\n")
	}
	if d := m.SubstantiveAfter(); d > 0 {
		fmt.Fprintf(&b, "substantive answer after: %s (target %s)\n", d.Round(time.Millisecond), profile.SubstantiveTarget)
	} else {
		b.WriteString("substantive answer after: not reached\n")
	}

	tokens := m.Tokens()
	b.WriteString("tokens by model and class:\n")
	for _, id := range tokens.Models() {
		usage := tokens.Usage(id)
		fmt.Fprintf(&b, "  %s: %s\n", id, usage)
	}
	if len(tokens.Models()) == 0 {
		b.WriteString("  (no model call was made)\n")
	}

	writeCounts(&b, "calls by worker", m.CallsByWorker())
	writeCounts(&b, "calls by backend", m.CallsByBackend())
	writeCounts(&b, "calls by cost class", m.CallsByCostClass())

	if shares := m.QuotaShareUsed(); len(shares) > 0 {
		b.WriteString("quota share consumed:\n")
		for _, backend := range sortedKeysFloat(shares) {
			fmt.Fprintf(&b, "  %s: %.6f of %d reported remaining\n",
				backend, shares[backend], m.RemainingQuotaObserved()[backend])
		}
	}
	if fallbacks := m.QuotaFallbacks(); len(fallbacks) > 0 {
		fmt.Fprintf(&b, "quota not reported by: %s — the absolute per-backend budget governed instead\n",
			strings.Join(fallbacks, ", "))
	}
	if intents := m.Intents(); len(intents) > 0 {
		fmt.Fprintf(&b, "calls not issued because a budget would have been breached: %d\n", len(intents))
		for _, intent := range intents {
			fmt.Fprintf(&b, "  %s — %s\n", intent.Budget, intent.Reason)
		}
	}
	return b.String()
}

func writeCounts(b *strings.Builder, title string, counts map[string]int64) {
	fmt.Fprintf(b, "%s:\n", title)
	if len(counts) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "  %s: %d\n", key, counts[key])
	}
}

func sortedKeysFloat(in map[string]float64) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
