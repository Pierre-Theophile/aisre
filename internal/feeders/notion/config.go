// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
)

// The connector's configuration (contracts/notion-connector.md §4, data-model "Database binding";
// 007 T017).

// MaxSummaryProperties is FR-021's limit.
const MaxSummaryProperties = 3

// Kinds a binding may feed: of the four published kinds, the two a Notion database holds.
var Kinds = []string{"postmortem", "runbook"}

// Defaults.
const (
	DefaultPollInterval   = 15 * time.Minute
	DefaultReconcileEvery = 24 * time.Hour
)

// Config is the operator's configuration.
type Config struct {
	// Workspace is the label in the source id, `notion:<workspace>`; it must be the integration's
	// workspace name.
	Workspace string `yaml:"workspace"`
	// Principal is who signs the configuration; recorded as `configured_by`.
	Principal string `yaml:"principal"`
	// AcknowledgeReadOnly must list `notion.capabilities` (ADR-0013 item 4).
	AcknowledgeReadOnly []string      `yaml:"acknowledge_read_only"`
	PollInterval        time.Duration `yaml:"poll_interval"`
	ReconcileEvery      time.Duration `yaml:"reconcile_every"`
	Databases           []Binding     `yaml:"databases"`
}

// Binding is one database the operator named.
type Binding struct {
	Database          string            `yaml:"database"`
	Kind              string            `yaml:"kind"`
	EntityProperty    string            `yaml:"entity_property"`
	Entities          map[string]string `yaml:"entities"`
	SummaryProperties []string          `yaml:"summary_properties"`
	AuthoredProperty  string            `yaml:"authored_property"`
	// IndexProperties are the named properties indexed as sections, one each, with no count limit
	// (008 FR-014). Absent means the summary properties.
	IndexProperties []string `yaml:"index_properties"`
	// IndexBody turns the page body's sections off for this database when false (008 FR-014).
	// Absent means on.
	IndexBody *bool `yaml:"index_body"`

	refs map[string]*graphv1.Ref
}

// LoadConfig reads and validates a configuration file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

// ParseConfig parses and validates a configuration.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("notion: configuration: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate refuses a configuration the connector could not honour, naming the clause.
func (c *Config) Validate() error {
	c.Workspace = strings.TrimSpace(c.Workspace)
	switch {
	case c.Workspace == "":
		return errors.New("notion: configuration: workspace is required; it is the label in the source id notion:<workspace>")
	case strings.ContainsAny(c.Workspace, ":/ "):
		return fmt.Errorf("notion: configuration: workspace %q may not contain ':', '/' or spaces", c.Workspace)
	case strings.TrimSpace(c.Principal) == "":
		return errors.New("notion: configuration: principal is required; a link whose configuration nobody signed " +
			"is a citation with anonymous provenance (FR-033)")
	case len(c.Databases) == 0:
		return errors.New("notion: configuration: no databases; the connector reads only databases an operator names (FR-010)")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.ReconcileEvery <= 0 {
		c.ReconcileEvery = DefaultReconcileEvery
	}
	seen := map[string]bool{}
	for i := range c.Databases {
		b := &c.Databases[i]
		b.Database = NormaliseID(b.Database)
		where := fmt.Sprintf("notion: configuration: database %d (%s)", i+1, b.Database)
		switch {
		case b.Database == "":
			return fmt.Errorf("notion: configuration: database %d names no database id", i+1)
		case seen[b.Database]:
			return fmt.Errorf("%s is bound twice", where)
		case !validKind(b.Kind):
			return fmt.Errorf("%s: kind %q; a Notion database feeds one of %v", where, b.Kind, Kinds)
		case strings.TrimSpace(b.EntityProperty) == "":
			return fmt.Errorf("%s: entity_property is required; it is the property that says which services a page concerns (FR-030)", where)
		case len(b.SummaryProperties) > MaxSummaryProperties:
			return fmt.Errorf("%s: %d summary properties, at most %d (FR-021)", where, len(b.SummaryProperties), MaxSummaryProperties)
		}
		seen[b.Database] = true
		b.refs = map[string]*graphv1.Ref{}
		for value, spelled := range b.Entities {
			ref, err := graph.ParseRef(spelled)
			if err != nil {
				return fmt.Errorf("%s: entities[%q] = %q: %w", where, value, spelled, err)
			}
			b.refs[value] = ref.Proto()
		}
	}
	return nil
}

func validKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind && knowledge.ValidKind(kind) {
			return true
		}
	}
	return false
}

// Legacy reports whether the binding maps values to entities itself (007's `entities` map). Without
// one, the service catalog attributes the pages (009 FR-006).
func (b *Binding) Legacy() bool { return len(b.Entities) > 0 }

// properties are the property names the binding reads, in configuration order.
func (b *Binding) properties() []string {
	names := append([]string{b.EntityProperty}, b.SummaryProperties...)
	for _, name := range b.Indexed() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if b.AuthoredProperty != "" {
		names = append(names, b.AuthoredProperty)
	}
	return names
}

// Indexed are the property names indexed as sections: index_properties, else the summary
// properties.
func (b *Binding) Indexed() []string {
	if b.IndexProperties != nil {
		return b.IndexProperties
	}
	return b.SummaryProperties
}

// BodyIndexed reports whether the page body is indexed.
func (b *Binding) BodyIndexed() bool { return b.IndexBody == nil || *b.IndexBody }

// Sections reports whether the binding indexes anything beyond its summary.
func (b *Binding) Sections() bool { return b.BodyIndexed() || len(b.Indexed()) > 0 }

// Map returns the entity refs the values name, deduplicated in value order, and the values the map
// does not name.
func (b *Binding) Map(values []string) (refs []*graphv1.Ref, unmapped []string) {
	seen := map[string]bool{}
	for _, v := range values {
		ref, ok := b.refs[v]
		if !ok {
			ref, ok = b.refs[NormaliseID(v)]
		}
		if !ok {
			unmapped = append(unmapped, v)
			continue
		}
		key := graph.RefFromProto(ref).String()
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	return refs, unmapped
}

// NormaliseID spells a Notion id the way the API returns it: lower case, dashed 8-4-4-4-12. An id
// copied from a URL has no dashes.
func NormaliseID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	plain := strings.ReplaceAll(id, "-", "")
	if len(plain) != 32 {
		return id
	}
	for _, r := range plain {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return id
		}
	}
	return plain[:8] + "-" + plain[8:12] + "-" + plain[12:16] + "-" + plain[16:20] + "-" + plain[20:]
}

// UnrecordedPrincipal stands in for the signing principal in a recording: a principal is a person.
const UnrecordedPrincipal = "principal-not-recorded"

// ForRecording is the configuration a recording's events are derived under: the workspace label and
// each database id in the recording's vocabulary, and no principal. ident pseudonymises one
// identifier of the named kind ("workspace" or "database").
func (c *Config) ForRecording(ident func(kind, value string) (string, error)) (*Config, error) {
	out := *c
	var err error
	if out.Workspace, err = ident("workspace", c.Workspace); err != nil {
		return nil, err
	}
	out.Principal = UnrecordedPrincipal
	out.AcknowledgeReadOnly = append([]string(nil), c.AcknowledgeReadOnly...)
	out.Databases = make([]Binding, len(c.Databases))
	for i, b := range c.Databases {
		b.Entities = maps.Clone(b.Entities)
		b.SummaryProperties = append([]string(nil), b.SummaryProperties...)
		if b.IndexProperties != nil {
			b.IndexProperties = append([]string(nil), b.IndexProperties...)
		}
		if b.IndexBody != nil {
			on := *b.IndexBody
			b.IndexBody = &on
		}
		if b.Database, err = ident("database", b.Database); err != nil {
			return nil, err
		}
		out.Databases[i] = b
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}
