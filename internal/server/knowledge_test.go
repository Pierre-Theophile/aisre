// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	knowledgev1 "github.com/Pierre-Theophile/aisre/api/sreagent/knowledge/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/knowledge/v1/knowledgev1connect"
)

// PutSections (feature 008 FR-005, FR-006, research R7): IngestBatch's token and source scoping,
// plus namespace ownership and the limits of contracts §4.

const notionSource = "notion:LDMA"

func (ts *graphServer) knowledgeClient(token string) knowledgev1connect.KnowledgeServiceClient {
	return knowledgev1connect.NewKnowledgeServiceClient(h2cClient(token), ts.baseURL, connect.WithGRPC())
}

func putRequest(source, ref string, sections ...*knowledgev1.Section) *connect.Request[knowledgev1.PutSectionsRequest] {
	return connect.NewRequest(&knowledgev1.PutSectionsRequest{SourceId: source, DocumentRef: ref, Sections: sections})
}

func symptoms(text string) *knowledgev1.Section {
	return &knowledgev1.Section{Label: "Signals / Symptoms", Text: text, Origin: "property"}
}

func TestPutSectionsStoresAndIsIdempotent(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.knowledgeClient(ts.feederToken(t, notionSource))
	ref := "notion.page=0001"

	resp, err := client.PutSections(context.Background(), putRequest(notionSource, ref,
		symptoms("checkout p99 over 4s after the rollout"),
		&knowledgev1.Section{Label: "Timeline", Text: "14:02 page fired", Origin: "body"}))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := resp.Msg; got.GetOpened() != 2 || got.GetClosed() != 0 || got.GetUnchanged() != 0 {
		t.Fatalf("first put = %v, want 2 opened", got)
	}

	resp, err = client.PutSections(context.Background(), putRequest(notionSource, ref,
		symptoms("checkout p99 over 4s after the rollout"),
		&knowledgev1.Section{Label: "Timeline", Text: "14:02 page fired", Origin: "body"}))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if got := resp.Msg; got.GetUnchanged() != 2 || got.GetOpened() != 0 {
		t.Fatalf("second put = %v, want 2 unchanged", got)
	}

	hits, err := ts.knowledge.Lexical(context.Background(), "rollout", []string{ref}, time.Now(), 10)
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	if len(hits) != 1 || hits[0].Label != "Signals / Symptoms" {
		t.Fatalf("hits = %+v, want the symptoms section", hits)
	}
}

func TestPutSectionsRefusals(t *testing.T) {
	ts := newGraphServer(t)
	feeder := ts.knowledgeClient(ts.feederToken(t, notionSource))
	ok := symptoms("checkout p99 over 4s")

	many := make([]*knowledgev1.Section, 501)
	for i := range many {
		many[i] = &knowledgev1.Section{Label: "Body", Part: uint32(i), Text: "x y", Origin: "body"} //nolint:gosec // test
	}

	cases := []struct {
		name   string
		client knowledgev1connect.KnowledgeServiceClient
		req    *connect.Request[knowledgev1.PutSectionsRequest]
		want   connect.Code
	}{
		{"reader token", ts.knowledgeClient(ts.readerToken(t)), putRequest(notionSource, "notion.page=1", ok), connect.CodePermissionDenied},
		{"another source", feeder, putRequest("notion:OTHER", "notion.page=1", ok), connect.CodePermissionDenied},
		{"another namespace", feeder, putRequest(notionSource, "github.file=ops/runbook.md", ok), connect.CodePermissionDenied},
		{"a lookalike namespace", feeder, putRequest(notionSource, "notionx.page=1", ok), connect.CodePermissionDenied},
		{"no document", feeder, putRequest(notionSource, "", ok), connect.CodeInvalidArgument},
		{"empty label", feeder, putRequest(notionSource, "notion.page=1", &knowledgev1.Section{Text: "x", Origin: "body"}), connect.CodeInvalidArgument},
		{"over 1,200 characters", feeder, putRequest(notionSource, "notion.page=1", symptoms(strings.Repeat("a", 1201))), connect.CodeInvalidArgument},
		{"over 500 sections", feeder, putRequest(notionSource, "notion.page=1", many...), connect.CodeInvalidArgument},
		{"a synthesised origin", feeder, putRequest(notionSource, "notion.page=1", &knowledgev1.Section{Label: "Summary", Text: "x", Origin: "summary"}), connect.CodeInvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.client.PutSections(context.Background(), c.req)
			if got := connect.CodeOf(err); got != c.want {
				t.Fatalf("code = %v (%v), want %v", got, err, c.want)
			}
		})
	}
}
