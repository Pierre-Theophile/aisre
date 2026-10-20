// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

func TestListenFromUsesPortOnlyWithoutAnExplicitListen(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		flagSet bool
		port    string
		want    string
	}{
		{"default without PORT", ":8080", false, "", ":8080"},
		{"PORT on a container platform", ":8080", false, "9000", ":9000"},
		{"PORT with stray spaces", ":8080", false, " 9000 ", ":9000"},
		{"explicit --listen wins over PORT", "127.0.0.1:7000", true, "9000", "127.0.0.1:7000"},
		{"explicit --listen without PORT", "127.0.0.1:7000", true, "", "127.0.0.1:7000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenFrom(tc.flag, tc.flagSet, tc.port); got != tc.want {
				t.Fatalf("listenFrom(%q, %v, %q) = %q, want %q", tc.flag, tc.flagSet, tc.port, got, tc.want)
			}
		})
	}
}
