// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

func TestNewID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		parts  []string
		want   string
	}{
		{
			name:   "the shape the fixtures use",
			source: "k8s:demo",
			parts:  []string{"deploy", "shop/checkout@rv1001"},
			want:   "k8s:demo:deploy:shop/checkout@rv1001",
		},
		{
			name:   "an otel window key",
			source: "otel:demo",
			parts:  []string{"edge", "checkout->payments@w1300"},
			want:   "otel:demo:edge:checkout->payments@w1300",
		},
		{
			name:   "empty parts are dropped rather than doubling the separator",
			source: "k8s:demo",
			parts:  []string{"deploy", "", "x"},
			want:   "k8s:demo:deploy:x",
		},
		{
			name:   "a newline in a source-supplied name cannot break the events file",
			source: "k8s:demo",
			parts:  []string{"deploy", "shop/check\nout"},
			want:   "k8s:demo:deploy:shop/check_out",
		},
		{
			name:   "a pipe cannot be confused with a resolution pair key",
			source: "k8s:demo",
			parts:  []string{"claim", "a|b"},
			want:   "k8s:demo:claim:a_b",
		},
		{
			name:   "a NUL cannot collide with the id hash separator",
			source: "k8s:demo",
			parts:  []string{"deploy", "a\x00b"},
			want:   "k8s:demo:deploy:a_b",
		},
		{
			name:   "surrounding whitespace is trimmed",
			source: " k8s:demo ",
			parts:  []string{"  deploy  "},
			want:   "k8s:demo:deploy",
		},
		{
			name:   "no parts is just the source",
			source: "k8s:demo",
			want:   "k8s:demo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := feeder.NewID(tc.source, tc.parts...); got != tc.want {
				t.Errorf("NewID(%q, %q) = %q, want %q", tc.source, tc.parts, got, tc.want)
			}
		})
	}
}

func TestNewIDIsDeterministic(t *testing.T) {
	t.Parallel()
	// The whole point of the helper: the same source-native identifiers must produce the same
	// id on every run and in every process (FR-020).
	first := feeder.NewID("k8s:demo", "deploy", "shop/checkout@rv1001")
	second := feeder.NewID("k8s:demo", "deploy", "shop/checkout@rv1001")
	if first != second {
		t.Fatalf("NewID is not deterministic: %q then %q", first, second)
	}
}

func TestIdempotencyKey(t *testing.T) {
	t.Parallel()
	const eventID = "k8s:demo:deploy:shop/checkout@rv1001"

	if got := feeder.IdempotencyKey(eventID); got != eventID {
		t.Errorf("IdempotencyKey(%q) = %q, want the event id itself", eventID, got)
	}
	if got, want := feeder.IdempotencyKey(eventID, "resync"), eventID+":resync"; got != want {
		t.Errorf("IdempotencyKey with a part = %q, want %q", got, want)
	}
}

func TestCheckpointID(t *testing.T) {
	t.Parallel()
	desc := feeder.Description{SourceID: "k8s:demo", Kind: "k8s"}
	to := mustTime(t, "2026-09-01T13:01:20Z")
	// The spelling the baseline fixture records.
	if got, want := feeder.CheckpointID(desc, to), "k8s:demo:ckpt:20260901T130120Z"; got != want {
		t.Errorf("CheckpointID = %q, want %q", got, want)
	}
}
