// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The Datadog table satisfies every guarantee Validate enforces: people dropped, pseudonyms typed,
// unlisted tags dropped (005 T079).
func TestTheDatadogPolicyValidates(t *testing.T) {
	t.Parallel()
	if err := sanitise.DatadogPolicy().Validate(); err != nil {
		t.Fatalf("the Datadog policy does not validate: %v", err)
	}
	for _, path := range []string{sanitise.DatadogPathUserEmail, sanitise.DatadogPathMonitorOwner} {
		if !sanitise.IsPeopleField(path) {
			t.Errorf("%s is not recognised as a person; Validate could not hold it to Dropped", path)
		}
	}
}
