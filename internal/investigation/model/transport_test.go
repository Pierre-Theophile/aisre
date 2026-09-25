// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
)

// The transport seam (tasks.md T060; FR-041, FR-042a, plan §Replay design).
//
// This is what Phase 7's CI gate is built on, so what is asserted here is exactly what that gate
// needs: a run's exchanges replay byte-identically through a strict transport with no network, and
// the first request that differs fails loudly naming the record it diverged at.

// TestARecordingReplaysWithNoNetwork: the same requests against the recorded exchanges return the
// recorded responses, and nothing is issued.
func TestARecordingReplaysWithNoNetwork(t *testing.T) {
	t.Parallel()

	first, err := modeltest.Transport(investigatorModel,
		modeltest.Turn{Text: "turn one", Usage: model.Usage{Input: 100, Output: 10}},
		modeltest.Turn{Text: "turn two", Usage: model.Usage{Input: 20, CacheRead: 100, Output: 10}},
	)
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, first)

	req := request()
	one, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete 1: %v", err)
	}
	req.Messages = append(req.Messages, one.AssistantMessage(), model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: "go on"}},
	})
	two, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete 2: %v", err)
	}

	recorded := first.Exchanges()
	if len(recorded) != 2 {
		t.Fatalf("recorded %d exchanges, want 2", len(recorded))
	}
	if recorded[0].RequestDigest == recorded[1].RequestDigest {
		t.Error("two different turns produced the same request digest")
	}

	// Replay: same engine, same requests, a strict transport built from the recording.
	replaying := model.NewReplayingTransport(recorded)
	if replaying.Mode() != model.TransportReplaying {
		t.Fatalf("mode = %s, want replaying", replaying.Mode())
	}
	replayClient := clientWith(t, replaying)

	replayReq := request()
	replayOne, err := replayClient.Complete(context.Background(), replayReq)
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	if replayOne.ResponseDigest != one.ResponseDigest {
		t.Errorf("replayed response digest %s != recorded %s", replayOne.ResponseDigest, one.ResponseDigest)
	}
	replayReq.Messages = append(replayReq.Messages, replayOne.AssistantMessage(), model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: "go on"}},
	})
	replayTwo, err := replayClient.Complete(context.Background(), replayReq)
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	if replayTwo.Text() != two.Text() || replayTwo.ResponseDigest != two.ResponseDigest {
		t.Errorf("replayed turn 2 is not the recorded turn 2")
	}
	if replayTwo.Usage.CacheRead != 100 {
		t.Errorf("cache read = %d, want the recorded 100", replayTwo.Usage.CacheRead)
	}
}

// TestAReplayFailsLoudlyOnTheFirstDivergingRecord (FR-041). A replay that improvises is evidence
// of nothing, so the failure names the record, both digests and the recorded body.
func TestAReplayFailsLoudlyOnTheFirstDivergingRecord(t *testing.T) {
	t.Parallel()

	recording, err := modeltest.Transport(investigatorModel,
		modeltest.Turn{Text: "turn one"},
		modeltest.Turn{Text: "turn two"},
	)
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, recording)

	req := request()
	one, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete 1: %v", err)
	}
	req.Messages = append(req.Messages, one.AssistantMessage(), model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: "go on"}},
	})
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete 2: %v", err)
	}

	replayClient := clientWith(t, model.NewReplayingTransport(recording.Exchanges()))

	// The first turn replays.
	replayReq := request()
	replayOne, err := replayClient.Complete(context.Background(), replayReq)
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}

	// The second diverges: the engine asks a different question.
	replayReq.Messages = append(replayReq.Messages, replayOne.AssistantMessage(), model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: "actually, ask something else"}},
	})
	_, err = replayClient.Complete(context.Background(), replayReq)
	if err == nil {
		t.Fatal("a diverging request was served from the recording")
	}
	divergence := model.DivergenceOf(err)
	if divergence == nil {
		t.Fatalf("the failure is not a typed divergence: %v", err)
	}
	if divergence.Seq != 2 {
		t.Errorf("diverged at record %d, want 2 — the *first* diverging record", divergence.Seq)
	}
	if divergence.Expected == "" || divergence.Actual == "" || divergence.Expected == divergence.Actual {
		t.Errorf("the divergence does not carry both digests: %+v", divergence)
	}
	if !strings.Contains(divergence.Error(), "record 2") {
		t.Errorf("the message does not name the record: %q", divergence.Error())
	}
}

// TestAReplayThatRunsOutOfRecordingSaysSo: a run that asks more questions than the recording holds
// is a divergence, not a silent end.
func TestAReplayThatRunsOutOfRecordingSaysSo(t *testing.T) {
	t.Parallel()

	recording, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "only turn"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, recording)
	req := request()
	one, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	replayClient := clientWith(t, model.NewReplayingTransport(recording.Exchanges()))
	replayReq := request()
	if _, err := replayClient.Complete(context.Background(), replayReq); err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	replayReq.Messages = append(replayReq.Messages, one.AssistantMessage(), model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: "another question"}},
	})
	_, err = replayClient.Complete(context.Background(), replayReq)
	divergence := model.DivergenceOf(err)
	if divergence == nil {
		t.Fatalf("running past the end of the recording was not a divergence: %v", err)
	}
	if !strings.Contains(divergence.Detail, "the recording holds 1") {
		t.Errorf("the divergence does not say the recording ran out: %q", divergence.Detail)
	}
}

// TestTheDigestIsOverCanonicalJSON: two requests that differ only in key order are the same
// request, so a replay does not break on the SDK's map iteration.
func TestTheDigestIsOverCanonicalJSON(t *testing.T) {
	t.Parallel()

	one, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "a"})
	if err != nil {
		t.Fatalf("canned: %v", err)
	}
	two, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "a"})
	if err != nil {
		t.Fatalf("canned: %v", err)
	}

	firstClient := clientWith(t, one)
	secondClient := clientWith(t, two)
	if _, err := firstClient.Complete(context.Background(), request()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := secondClient.Complete(context.Background(), request()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if one.Exchanges()[0].RequestDigest != two.Exchanges()[0].RequestDigest {
		t.Error("two identical requests produced different digests; a replay would break on nothing")
	}
}
