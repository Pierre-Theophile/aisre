// SPDX-License-Identifier: Apache-2.0

// Package expectations loads the published expectation table (002 FR-022b, plan V6, T124): for
// each cause category, the signal families that are decisive for it, and the mode the table is in.
//
// The table is read by the examination report, which names a decisive family that was reachable
// for a hypothesis and never queried. In `observe` mode that is all it does. In `enforce` mode
// (T132, only after ADR-0019 D4's dual gate) such a hypothesis is held at inconclusive.
package expectations

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The two modes.
const (
	// ModeObserve reports omissions and holds nothing.
	ModeObserve = "observe"
	// ModeEnforce holds a hypothesis at inconclusive while a decisive family is reachable and
	// unqueried.
	ModeEnforce = "enforce"
)

// Families are the signal families a row may name: the search-space profile's, less `changes`,
// which every change hypothesis already rests on.
var Families = []string{"metrics", "logs", "traces", "knowledge"}

// Categories are the published cause categories: 001's change kinds in lower case, then the two
// categories FR-022b adds. A table must name every one of them, so a new change kind cannot be
// added without someone deciding what is decisive for it.
var Categories = []string{
	"rollout", "iac_apply", "flag_flip", "secret_rotation", "scaling", "migration", "dns_switch",
	"cloud_maintenance", "config_change", "deprecation", "vendor_incident", "iam_change",
	"quota_change", "other",
	"not_change_induced", "stopped_flow",
}

// Row is one category's expectation.
type Row struct {
	// Decisive are the families an investigation is expected to have queried.
	Decisive []string `yaml:"decisive"`
	// Why is the sentence a reader is given for the row.
	Why string `yaml:"why"`
}

// Table is one version of the expectation table.
type Table struct {
	Version    int            `yaml:"version"`
	Mode       string         `yaml:"mode"`
	Categories map[string]Row `yaml:"categories"`
}

// published is version 1, compiled in so that a deployment needs no file to get the published
// table. config/expectations/v1.yaml is its checked-in copy for operators to read, and a test
// keeps the two byte-identical.
//
//go:embed v1.yaml
var published []byte

// Published returns the table in force: version 1. It panics only if the compiled-in table is
// invalid, which a test makes impossible to ship.
func Published() *Table {
	t, err := Parse(published)
	if err != nil {
		panic(err)
	}
	return t
}

// Load reads and validates a table.
func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("expectations: read %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse decodes and validates a table. An unknown key is a typo, and a typo that is ignored is a
// row nobody chose.
func Parse(raw []byte) (*Table, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var t Table
	if err := decoder.Decode(&t); err != nil {
		return nil, fmt.Errorf("expectations: parse: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks the version, the mode, that every published category has a row and no other
// does, and that every row names only published families, once each, with a reason.
func (t *Table) Validate() error {
	if t.Version < 1 {
		return fmt.Errorf("expectations: version %d; versions start at 1", t.Version)
	}
	if t.Mode != ModeObserve && t.Mode != ModeEnforce {
		return fmt.Errorf("expectations: mode %q; want %s or %s", t.Mode, ModeObserve, ModeEnforce)
	}
	known := map[string]bool{}
	for _, c := range Categories {
		known[c] = true
		if _, ok := t.Categories[c]; !ok {
			return fmt.Errorf("expectations: category %s has no row; every published category needs a decision", c)
		}
	}
	families := map[string]bool{}
	for _, f := range Families {
		families[f] = true
	}
	for name, row := range t.Categories {
		if !known[name] {
			return fmt.Errorf("expectations: %s is not a published cause category", name)
		}
		if strings.TrimSpace(row.Why) == "" {
			return fmt.Errorf("expectations: category %s gives no reason", name)
		}
		seen := map[string]bool{}
		for _, f := range row.Decisive {
			if !families[f] {
				return fmt.Errorf("expectations: category %s names %q, not one of %v", name, f, Families)
			}
			if seen[f] {
				return fmt.Errorf("expectations: category %s names %s twice", name, f)
			}
			seen[f] = true
		}
	}
	return nil
}

// Decisive returns the decisive families for a category, sorted, and whether the table has the
// category at all.
func (t *Table) Decisive(category string) ([]string, bool) {
	row, ok := t.Categories[category]
	if !ok {
		return nil, false
	}
	out := append([]string(nil), row.Decisive...)
	sort.Strings(out)
	return out, true
}

// Label is how the table is named where it is recorded: `v1 (observe)`.
func (t *Table) Label() string { return fmt.Sprintf("v%d (%s)", t.Version, t.Mode) }
