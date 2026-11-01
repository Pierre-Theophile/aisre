// SPDX-License-Identifier: Apache-2.0

package record

import (
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

// Incident is one incident row.
type Incident struct {
	// IncidentID is the deterministic incident identifier (intake.IncidentID).
	IncidentID string
	// CanonicalSubjectID is the graph entity the first symptom resolved to. Empty is permitted
	// and means "the first symptom resolved to nothing", which is the FR-002b path.
	CanonicalSubjectID string
	// OpenedAt is the first symptom's instant; LastSymptomAt moves as symptoms attach.
	OpenedAt      time.Time
	LastSymptomAt time.Time
	// AssociationRuleVersion is the published rule that grouped symptoms into this incident.
	AssociationRuleVersion string
}

// NewInvestigation is everything needed to open a run.
type NewInvestigation struct {
	// InvestigationID is the stable identifier printed in every rendering.
	InvestigationID string
	// IncidentID is the incident it covers.
	IncidentID string
	// ReopensInvestigationID links a reopened run to its parent (FR-057b). Empty on a first run.
	ReopensInvestigationID string
	// ValidAt, ObservedAt, ReviewMode and Window come from intake.
	ValidAt     time.Time
	ObservedAt  time.Time
	ReviewMode  bool
	WindowStart time.Time
	WindowEnd   time.Time
	// Profile is the budget profile applied (FR-006).
	Profile string
	// Requester is the authenticated identity that asked (FR-066). Required: the schema's FK
	// into graph.principals is what makes `anonymous_principal` unrepresentable.
	Requester string
	// ModelConfig is the recorded production model configuration (FR-061).
	ModelConfig *structpb.Struct
	// AlgebraVersion, LedgerRuleVersion and SchemaVersion are the published versions in force.
	AlgebraVersion    string
	LedgerRuleVersion string
	SchemaVersion     string
	// StartedAt is when the run began. Zero means now.
	StartedAt time.Time
}

// ErrAnonymousPrincipal is an investigation with no authenticated requester (FR-066). It is the
// `anonymous_principal` reason code of data-model §Validation rules.
var ErrAnonymousPrincipal = errors.New("anonymous_principal: an investigation must name the authenticated identity that requested it (FR-066)")

// ListFilter narrows List (contracts/cli.md `investigate list`).
type ListFilter struct {
	// IncidentID lists the runs covering one incident, including reopens.
	IncidentID string
	// Lifecycle lists the runs in one status.
	Lifecycle string
	// Since lists the runs started at or after an instant.
	Since time.Time
	// Limit bounds the page; zero means DefaultListLimit.
	Limit int
}

// DefaultListLimit is the page size `investigate list` uses when none is given.
const DefaultListLimit = 50
