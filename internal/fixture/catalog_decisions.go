// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"fmt"
	"strconv"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The catalog's human decisions in a fixture manifest (feature 009, FR-012, FR-041).
//
// A catalog decision is a person's, exactly as a merge is, so it is replayed the same way: from
// `human_decisions`, with its principal and its instant, under the deterministic event id the
// server would have given it (internal/server/resolution.go spells the same kinds and subjects).
//
//	human_decisions:
//	  - catalog_entry: {slug: candidate-view, name: Candidate View, kind: service}
//	  - alias: {name: datadog.log_service=candidate-api, entry: candidate-view}
//	  - retract_alias: {name: ..., environment: production, entry: candidate-view}
//	  - reject_alias: {name: ..., entry: candidate-view}   # a proposal a person said no to
//	  - correct: {signal: datadog.monitor=1, entries: [infra]}
//	  - correct: {signal: datadog.monitor=1, entries: [infra], name: datadog.log_service=candidate-api}
//	  - revert: "#3"            # the correction at human_decisions[3], or its event id
//
// each with `at`, `principal` and `reason` like every other decision.

// CatalogEntryDecision declares or restates an entry.
type CatalogEntryDecision struct {
	Slug        string `yaml:"slug"`
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"`
	Status      string `yaml:"status"`
	Owner       string `yaml:"owner"`
	Description string `yaml:"description"`
}

// AliasDecision states or retracts that a name denotes an entry ("-" for a hidden name).
type AliasDecision struct {
	Name        string `yaml:"name"`
	Environment string `yaml:"environment"`
	Entry       string `yaml:"entry"`
}

// CorrectionDecision corrects a signal's attribution; with Name, every signal carrying the name.
type CorrectionDecision struct {
	Signal      string   `yaml:"signal"`
	Entries     []string `yaml:"entries"`
	Name        string   `yaml:"name"`
	Environment string   `yaml:"environment"`
}

// catalogKind is the catalog decision a human_decisions entry names, its subject as the server
// spells it, and how many catalog forms it names.
func (d HumanDecision) catalogKind() (kind string, subject []string, set int) {
	if e := d.CatalogEntry; e != nil {
		kind, subject, set = "declare_catalog_entry",
			[]string{e.Slug, e.Name, e.kind(), e.status(), e.Owner, e.Description}, set+1
	}
	if a := d.Alias; a != nil {
		kind, subject, set = "declare_alias", []string{a.Name, a.Environment, a.Entry}, set+1
	}
	if a := d.RetractAlias; a != nil {
		kind, subject, set = "retract_alias", []string{a.Name, a.Environment, a.Entry}, set+1
	}
	if a := d.RejectAlias; a != nil {
		kind, subject, set = "reject_alias", []string{a.Name, a.Environment, a.Entry}, set+1
	}
	if c := d.Correct; c != nil {
		scope := "signal"
		if c.Name != "" {
			scope = "name"
		}
		subject = append([]string{c.Signal, scope}, c.Entries...)
		if c.Name != "" {
			subject = append(subject, c.Name, c.Environment, "")
		}
		kind, set = "correct_attribution", set+1
	}
	if d.Revert != "" {
		kind, subject, set = "revert_correction", []string{d.Revert}, set+1
	}
	return kind, subject, set
}

func (e *CatalogEntryDecision) kind() string {
	if e.Kind == "" {
		return graph.CatalogKindService
	}
	return e.Kind
}

func (e *CatalogEntryDecision) status() string {
	if e.Status == "" {
		return graph.CatalogStatusConfirmed
	}
	return e.Status
}

// resolveRevert turns a `#n` reference into the event id of human_decisions[n].
func resolveRevert(raw string, ids []string, at int) (string, error) {
	n, ok := strings.CutPrefix(strings.TrimSpace(raw), "#")
	if !ok {
		return raw, nil
	}
	i, err := strconv.Atoi(n)
	if err != nil || i < 0 || i >= at {
		return "", fmt.Errorf("revert %q: name an earlier human_decisions entry as #<index>", raw)
	}
	return ids[i], nil
}

// setCatalogBody fills in the event body of a catalog decision.
func (d HumanDecision) setCatalogBody(env *graphv1.EventEnvelope, kind string, subject []string) error {
	parse := func(raw string) (*graphv1.Ref, error) {
		ref, err := graph.ParseRef(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%q: %w", raw, err)
		}
		return ref.Proto(), nil
	}
	switch kind {
	case "declare_catalog_entry":
		e := d.CatalogEntry
		env.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: &graphv1.DeclareCatalogEntry{
			Slug: e.Slug, DisplayName: e.Name, Kind: e.kind(), Status: e.status(), Owner: e.Owner,
			Description: e.Description, Rationale: d.Reason,
		}}
	case "declare_alias", "retract_alias", "reject_alias":
		a := d.Alias
		switch kind {
		case "retract_alias":
			a = d.RetractAlias
		case "reject_alias":
			a = d.RejectAlias
		}
		name, err := parse(a.Name)
		if err != nil {
			return err
		}
		switch kind {
		case "declare_alias":
			env.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: &graphv1.DeclareAlias{
				Name: name, Environment: a.Environment, EntrySlug: a.Entry, Rationale: d.Reason,
			}}
		case "retract_alias":
			env.Body = &graphv1.EventEnvelope_RetractAlias{RetractAlias: &graphv1.RetractAlias{
				Name: name, Environment: a.Environment, EntrySlug: a.Entry, Rationale: d.Reason,
			}}
		default:
			env.Body = &graphv1.EventEnvelope_RejectAlias{RejectAlias: &graphv1.RejectAlias{
				Name: name, Environment: a.Environment, EntrySlug: a.Entry, Rationale: d.Reason,
			}}
		}
	case "correct_attribution":
		c := d.Correct
		signal, err := parse(c.Signal)
		if err != nil {
			return err
		}
		body := &graphv1.CorrectAttribution{
			Signal: signal, EntrySlugs: c.Entries, Scope: "signal", Environment: c.Environment, Rationale: d.Reason,
		}
		if c.Name != "" {
			if body.Name, err = parse(c.Name); err != nil {
				return err
			}
			body.Scope = "name"
		}
		env.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: body}
	case "revert_correction":
		env.Body = &graphv1.EventEnvelope_RevertCorrection{RevertCorrection: &graphv1.RevertCorrection{
			CorrectionEventId: subject[0], Rationale: d.Reason,
		}}
	default:
		return fmt.Errorf("unknown decision kind %q", kind)
	}
	return nil
}
