// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strings"
	"time"
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
	// Classifier turns layer 3 on (009 research R6). Absent, no classifier runs and attribution
	// needs no network and no model (FR-034).
	Classifier *ClassifierConfig `yaml:"classifier,omitempty"`
}

// ClassifierKindEmbedding is the shipped classifier: the cosine of 008's embeddings, through the
// `embedding:` block's model.
const ClassifierKindEmbedding = "embedding"

// ClassifierConfig is layer 3 as deployed.
type ClassifierConfig struct {
	// Kind names the classifier; `embedding` is the one shipped. Another is plugged in through
	// pkg/attribution.Classifier and catalog.WithClassifier.
	Kind string `yaml:"kind"`
	// Timeout bounds one call; 2s when absent. A call that runs over leaves the decision to
	// layers 0–2 (FR-035).
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// Calibration maps the classifier's raw score to a probability, 1 / (1 + e^-(slope·x +
	// intercept)); both are fitted on the labelled set for the configured model.
	Calibration *ClassifierCalibration `yaml:"calibration"`
}

// ClassifierCalibration is a logistic calibration's two parameters.
type ClassifierCalibration struct {
	Slope     float64 `yaml:"slope"`
	Intercept float64 `yaml:"intercept"`
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
	if c := a.Classifier; c != nil {
		switch {
		case c.Kind != ClassifierKindEmbedding:
			return fmt.Errorf("attribution: classifier kind %q: the shipped one is %q", c.Kind, ClassifierKindEmbedding)
		case c.Timeout < 0:
			return fmt.Errorf("attribution: classifier timeout %s is negative", c.Timeout)
		case c.Calibration == nil || c.Calibration.Slope <= 0:
			return fmt.Errorf("attribution: classifier calibration needs a positive slope: a score that does not rise with the cosine is not calibrated")
		}
	}
	return nil
}
