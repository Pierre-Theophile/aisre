// SPDX-License-Identifier: Apache-2.0

package classify

import (
	"context"
	"errors"

	"github.com/Pierre-Theophile/aisre/pkg/attribution"
)

// Unavailable is a configured classifier that cannot run, typically because its credential is
// missing. Every call fails with the reason, so each decision records that layers 0–2 decided
// without it (FR-035), rather than the attributor silently running without layer 3.
type Unavailable struct {
	id, reason string
}

var _ attribution.Classifier = Unavailable{}

// NewUnavailable returns the stand-in for the classifier id, failing with reason.
func NewUnavailable(id, reason string) Unavailable { return Unavailable{id: id, reason: reason} }

// ID is the configured classifier's.
func (u Unavailable) ID() string { return u.id }

// Classify always fails.
func (u Unavailable) Classify(context.Context, attribution.Signal, []attribution.EntryProfile) ([]attribution.Scored, error) {
	return nil, errors.New("the classifier is unavailable: " + u.reason)
}
