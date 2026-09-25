// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"
)

// Usage accounting (T060, T072, FR-048, plan §Observability and cost accounting).
//
// Cost is recorded twice and the order matters. The primary unit is **tokens per model id per
// class** — the provider-independent number that still means the same thing after the vendor
// changes its prices twice. The monetary figure is derived from a versioned price table whose
// version is stored on the investigation, so a cost printed a year ago still says what it meant.
//
// Four classes, never three: a cached prefix is not an input prefix, and a cache *write* is not
// a cache *read*. Collapsing them would hide the one thing the prefix design exists to buy.

// The published token classes. They are the keys of `BudgetSpend.tokens_by_model_and_class`, so
// they are schema: a fixture asserts on these strings.
const (
	// ClassInput is uncached input.
	ClassInput = "input"
	// ClassCacheWrite is input written into the cache.
	ClassCacheWrite = "cache_write"
	// ClassCacheRead is input served from the cache.
	ClassCacheRead = "cache_read"
	// ClassOutput is generated tokens, thinking included.
	ClassOutput = "output"
)

// Classes is the published class set, in the order a rendering lists them.
var Classes = []string{ClassInput, ClassCacheWrite, ClassCacheRead, ClassOutput}

// Usage is one call's token accounting.
type Usage struct {
	// Input is uncached input tokens.
	Input int64
	// CacheWrite is tokens written into the cache.
	CacheWrite int64
	// CacheRead is tokens served from the cache. A zero on turn two means a silent invalidator
	// crept into the prefix, which is what the conformance check exists to catch (FR-061).
	CacheRead int64
	// Output is generated tokens.
	Output int64
}

func usageOf(u anthropic.BetaUsage) Usage {
	return Usage{
		Input:      u.InputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		Output:     u.OutputTokens,
	}
}

// ByClass returns the usage keyed by published class, omitting the classes that are zero so that
// a recorded map says what happened rather than padding it.
func (u Usage) ByClass() map[string]int64 {
	out := map[string]int64{}
	for class, value := range map[string]int64{
		ClassInput:      u.Input,
		ClassCacheWrite: u.CacheWrite,
		ClassCacheRead:  u.CacheRead,
		ClassOutput:     u.Output,
	} {
		if value != 0 {
			out[class] = value
		}
	}
	return out
}

// Total is every class added together, the single number a wall-clock-shaped budget spends.
func (u Usage) Total() int64 { return u.Input + u.CacheWrite + u.CacheRead + u.Output }

// Add accumulates another call's usage.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Input:      u.Input + other.Input,
		CacheWrite: u.CacheWrite + other.CacheWrite,
		CacheRead:  u.CacheRead + other.CacheRead,
		Output:     u.Output + other.Output,
	}
}

// String renders "input 1200, cache_write 9800, cache_read 0, output 640".
func (u Usage) String() string {
	byClass := map[string]int64{
		ClassInput: u.Input, ClassCacheWrite: u.CacheWrite,
		ClassCacheRead: u.CacheRead, ClassOutput: u.Output,
	}
	out := ""
	for i, class := range Classes {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s %d", class, byClass[class])
	}
	return out
}

// Cost returns the monetary figure for one model's usage under this client's price table, in the
// table's currency, rounded to six decimals.
//
// A model the table does not price is an error rather than a zero: an unpriced class silently
// costs nothing, which is the one answer that is never true.
func (c *Client) Cost(modelID string, u Usage) (float64, error) {
	price, ok := c.prices.Models[modelID]
	if !ok {
		return 0, fmt.Errorf("prices %s: model %s is not priced; a run that cannot price itself "+
			"cannot report its spend (FR-048)", c.prices.Version, modelID)
	}
	const perMillion = 1e6
	total := float64(u.Input)*price.Input +
		float64(u.CacheWrite)*price.CacheWrite5m +
		float64(u.CacheRead)*price.CacheRead +
		float64(u.Output)*price.Output
	return round6(total / perMillion), nil
}

func round6(v float64) float64 {
	scaled := v * 1e6
	if scaled < 0 {
		return float64(int64(scaled-0.5)) / 1e6
	}
	return float64(int64(scaled+0.5)) / 1e6
}

// TokenLedger accumulates usage per model id and per class across a run. It is what
// `BudgetSpend.tokens_by_model_and_class` is built from.
//
// It is keyed by model id rather than by role because the *model* is what a price applies to,
// and because two roles sharing one model must aggregate rather than double-count.
type TokenLedger struct {
	byModel map[string]Usage
}

// NewTokenLedger returns an empty ledger.
func NewTokenLedger() *TokenLedger { return &TokenLedger{byModel: map[string]Usage{}} }

// Record adds one call's usage.
func (t *TokenLedger) Record(modelID string, u Usage) {
	if t.byModel == nil {
		t.byModel = map[string]Usage{}
	}
	t.byModel[modelID] = t.byModel[modelID].Add(u)
}

// Models returns the model ids that have usage, sorted.
func (t *TokenLedger) Models() []string {
	out := make([]string, 0, len(t.byModel))
	for id := range t.byModel {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Usage returns one model's accumulated usage.
func (t *TokenLedger) Usage(modelID string) Usage { return t.byModel[modelID] }

// Total is every model's usage added together.
func (t *TokenLedger) Total() Usage {
	var out Usage
	for _, id := range t.Models() {
		out = out.Add(t.byModel[id])
	}
	return out
}

// Flat renders the ledger as the published `model_id:class → tokens` map, which is the
// provider-independent unit FR-048 asks for.
func (t *TokenLedger) Flat() map[string]int64 {
	out := map[string]int64{}
	for id, usage := range t.byModel {
		for class, value := range usage.ByClass() {
			out[id+":"+class] = value
		}
	}
	return out
}

// Cost prices the whole ledger against a price table.
func (t *TokenLedger) Cost(client *Client) (float64, error) {
	var total float64
	for _, id := range t.Models() {
		cost, err := client.Cost(id, t.byModel[id])
		if err != nil {
			return 0, err
		}
		total += cost
	}
	return round6(total), nil
}

// PriceTokens prices a whole token ledger. It is the budget package's `Client` interface, so a
// spend report can be rendered from a price table without being handed a live API client.
func (c *Client) PriceTokens(tokens *TokenLedger) (float64, error) { return tokens.Cost(c) }

// PriceTableVersion is the price table's version, stored on every investigation so a historical
// cost stays interpretable after prices move (FR-048).
func (c *Client) PriceTableVersion() string { return c.prices.Version }
