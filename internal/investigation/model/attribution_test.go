// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
	"time"
)

// The `attribution:` block (009 contracts §6, research R6).
func TestTheAttributionBlockIsValidated(t *testing.T) {
	t.Parallel()
	calibrated := &ClassifierCalibration{Slope: 8, Intercept: -4}
	for name, c := range map[string]struct {
		config AttributionConfig
		want   string
	}{
		"no ruleset":     {AttributionConfig{}, "ruleset is required"},
		"classifier":     {AttributionConfig{Ruleset: "r", Classifier: &ClassifierConfig{Kind: "embedding", Calibration: calibrated, Timeout: time.Second}}, ""},
		"unknown kind":   {AttributionConfig{Ruleset: "r", Classifier: &ClassifierConfig{Kind: "llm", Calibration: calibrated}}, "the shipped one"},
		"no calibration": {AttributionConfig{Ruleset: "r", Classifier: &ClassifierConfig{Kind: "embedding"}}, "positive slope"},
		"falling":        {AttributionConfig{Ruleset: "r", Classifier: &ClassifierConfig{Kind: "embedding", Calibration: &ClassifierCalibration{Slope: -1}}}, "positive slope"},
	} {
		err := c.config.Validate()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
