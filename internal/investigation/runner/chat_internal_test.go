// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
)

// TestChatChannelOfIsTheDeclaredChannelOnly (006 FR-031): only a declaration made in a Slack
// workspace has a chat channel, and only a channel inside that workspace.
func TestChatChannelOfIsTheDeclaredChannelOnly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		system, ref, want string
	}{
		{"slack:T0TWIN", "slack:T0TWIN/C0SEV2", "slack:T0TWIN/C0SEV2"},
		{"slack:T0TWIN", "slack:T0OTHER/C0SEV2", ""},
		{"slack:T0TWIN", "slack:T0TWIN/", ""},
		{"slack:T0TWIN", "", ""},
		{"monitor", "slack:T0TWIN/C0SEV2", ""},
		{"", "", ""},
	} {
		in := &intake.Intake{Symptom: &investigationv1.Symptom{OriginSystem: tc.system, OriginRef: tc.ref}}
		if got := chatChannelOf(in); got != tc.want {
			t.Errorf("%q %q: %q, want %q", tc.system, tc.ref, got, tc.want)
		}
	}
}
