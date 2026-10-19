// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strings"
)

// AttributionConfig is the service catalog's decision model as deployed (feature 009, contracts
// §6). Optional: without it the attributor runs the published ruleset with its published threshold
// and margin.
type AttributionConfig struct {
	// Ruleset names the decision model. A threshold or margin other than the published ones is a
	// different ruleset, and every attribution records which one decided it (009 FR-038).
	Ruleset string `yaml:"ruleset"`
	// Threshold is τ: the score at or above which layer 2 attributes on its own.
	Threshold *float64 `yaml:"threshold,omitempty"`
	// Margin is δ: how far an attributed entry must lead the best one below τ.
	Margin *float64 `yaml:"margin,omitempty"`
}

// Validate refuses a threshold or margin that is not a probability.
func (a AttributionConfig) Validate() error {
	if strings.TrimSpace(a.Ruleset) == "" {
		return fmt.Errorf("attribution: ruleset is required; it is recorded on every attribution")
	}
	if a.Threshold != nil && (*a.Threshold <= 0 || *a.Threshold > 1) {
		return fmt.Errorf("attribution: threshold %v: want a score in (0, 1]", *a.Threshold)
	}
	if a.Margin != nil && (*a.Margin < 0 || *a.Margin >= 1) {
		return fmt.Errorf("attribution: margin %v: want a score difference in [0, 1)", *a.Margin)
	}
	return nil
}
