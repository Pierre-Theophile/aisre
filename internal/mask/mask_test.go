// SPDX-License-Identifier: Apache-2.0

package mask_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/mask"
)

func TestLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{"email", "login failed for alice@example.com", "login failed for <email>"},
		{"ip", "connection refused from 10.1.2.3", "connection refused from <ip>"},
		{"uuid", "request 3f2504e0-4f89-11d3-9a0c-0305e82c3301 failed", "request <uuid> failed"},
		{"duration", "upstream took 251ms", "upstream took <dur>"},
		{"number", "returned status 503 after 3 retries", "returned status <num> after <num> retries"},
		{"url", "GET https://payments.internal/orders failed", "GET <url> failed"},
		{"already masked", "upstream took <dur>", "upstream took <dur>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mask.Line(tc.line)
			if got != tc.want {
				t.Errorf("Line(%q) = %q, want %q", tc.line, got, tc.want)
			}
			if again := mask.Line(got); again != got {
				t.Errorf("masking is not idempotent: %q → %q", got, again)
			}
		})
	}
}
