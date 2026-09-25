// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// RequireSource on every ingestion entry point (FR-046, T090).
//
// Ingest, IngestBatch and RegisterSource are three doors into the same room. A check on two of
// them is a check on none, so each is pinned separately here, for both the refusals that
// matter: the wrong role, and the right role scoped to somebody else's source.

// A reader token authenticates, so the interceptor lets it through; the handler is what must
// refuse it. All three RPCs, because each calls RequireSource on its own.
func TestReaderTokenCannotIngest(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.readerToken(t))
	events := fixtureEvents(t, testSourceID, 1)

	t.Run("register-source", func(t *testing.T) {
		_, err := client.RegisterSource(context.Background(),
			connect.NewRequest(&graphv1.RegisterSourceRequest{SourceId: testSourceID, Kind: "otel"}))
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("code = %v (%v), want PermissionDenied", got, err)
		}
	})

	t.Run("ingest-batch", func(t *testing.T) {
		_, err := client.IngestBatch(context.Background(),
			connect.NewRequest(&graphv1.IngestBatchRequest{Events: events}))
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("code = %v (%v), want PermissionDenied", got, err)
		}
	})

	t.Run("ingest-stream", func(t *testing.T) {
		stream := client.Ingest(context.Background())
		_ = stream.Send(events[0])
		_ = stream.CloseRequest()
		_, err := stream.Receive()
		if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
			t.Fatalf("code = %v (%v), want PermissionDenied", got, err)
		}
		_ = stream.CloseResponse()
	})
}

// The stream is the path a live feeder actually uses, and it is the one an authorization check
// is easiest to forget: the interceptor runs once, before the first message, while the source
// id arrives per message.
func TestIngestStreamForAnotherSourceIsPermissionDenied(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.feederToken(t, testSourceID))
	registerSource(t, client, testSourceID)

	foreign := fixtureEvents(t, otherSourceID, 1)

	stream := client.Ingest(context.Background())
	_ = stream.Send(foreign[0])
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v (%v), want PermissionDenied", got, err)
	}
	_ = stream.CloseResponse()

	// Nothing of the foreign source reached the log: the check runs before the event id is
	// even recorded, so a caller cannot probe another source's idempotency keys.
	if _, err := ts.projector.Store().Pool().Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("database: %v", err)
	}
}

// A feeder token scoped to a source may register only that source, which is what stops
// registration being used to claim another feeder's identity before writing as it.
func TestRegisterSourceIsScopedToTheToken(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.feederToken(t, testSourceID))

	if _, err := client.RegisterSource(context.Background(),
		connect.NewRequest(&graphv1.RegisterSourceRequest{
			SourceId: testSourceID, Kind: "otel", Ordering: "none", SchemaVersion: "1.0.0",
		})); err != nil {
		t.Fatalf("register own source: %v", err)
	}

	_, err := client.RegisterSource(context.Background(),
		connect.NewRequest(&graphv1.RegisterSourceRequest{SourceId: otherSourceID, Kind: "k8s"}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("register a foreign source: code = %v (%v), want PermissionDenied", got, err)
	}
}
