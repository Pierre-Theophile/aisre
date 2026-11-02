// SPDX-License-Identifier: Apache-2.0

package event_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
)

func TestStorePrecisionCutsEveryInstantToTheMicrosecond(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 15, 17, 56, 595_123_456, time.UTC)
	env := &graphv1.EventEnvelope{
		EventId:          "e",
		SourceObservedAt: timestamppb.New(at),
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			ValidAt: timestamppb.New(at.Add(time.Second)),
		}},
	}

	got := event.StorePrecision(env)

	if want := at.Truncate(time.Microsecond); !got.GetSourceObservedAt().AsTime().Equal(want) {
		t.Errorf("envelope instant %s, want %s", got.GetSourceObservedAt().AsTime(), want)
	}
	if want := at.Add(time.Second).Truncate(time.Microsecond); !got.GetUpsertNode().GetValidAt().AsTime().Equal(want) {
		t.Errorf("body instant %s, want %s", got.GetUpsertNode().GetValidAt().AsTime(), want)
	}
	if env.GetSourceObservedAt().GetNanos() != 595_123_456 || env.GetUpsertNode().GetValidAt().GetNanos() != 595_123_456 {
		t.Error("the envelope passed in was modified")
	}
}

func TestStorePrecisionLeavesAMicrosecondEnvelopeAlone(t *testing.T) {
	t.Parallel()
	env := &graphv1.EventEnvelope{SourceObservedAt: timestamppb.New(time.Date(2026, 11, 2, 15, 0, 0, 5_000, time.UTC))}
	if got := event.StorePrecision(env); got != env {
		t.Error("an envelope already at microsecond precision was copied")
	}
}
