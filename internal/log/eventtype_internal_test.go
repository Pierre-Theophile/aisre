// SPDX-License-Identifier: Apache-2.0

package log

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Every event body can be named, stored and read back (004 T148).
//
// ---------------------------------------------------------------------------------------------
// Why this test exists
//
// Storing an event and replaying one are three hand-written switches over the same oneof:
// `EventType` names the body, `BodyMessage` marshals it, and `setBody` reconstructs it. Nothing tied
// them together, and the comments in read.go say what happens when they drift — *"a type this switch
// does not know is a type that cannot be replayed, which is the same thing as a type that cannot be
// stored"* — twice, in two places, because it had been noticed twice.
//
// It happened a third time on the way to this file. `correlate_entity` was added to `Validate`, to the
// migration's type constraint, to `EventType` and to `BodyMessage`, and missed in `setBody`. Every unit
// test passed; every feeder emitted the event; the projector applied it. The whole thing appended
// cleanly to `log.events` and then failed on replay, and what caught it was a recorded fixture rather
// than anything in this package — which means it would have been caught by CI and not by review, on a
// branch where the fixtures had not yet been regenerated.
//
// So the switches are now tied together, by the only thing that knows the true list: the generated
// descriptor of the `body` oneof. A new event type fails this test until all three switches and the
// migration's constraint know it, which is the shape the comments were asking for.
//
// It lives in `package log` rather than `package log_test` because `setBody` is unexported, and
// exporting it for a test would widen the package's surface for nothing: the point is to check the
// three switches agree, and that is an internal property.

func TestEveryEventBodyIsNamedMarshalledAndReadBack(t *testing.T) {
	t.Parallel()

	oneof := (&graphv1.EventEnvelope{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	if oneof == nil {
		t.Fatal("EventEnvelope has no `body` oneof; this test is checking the wrong message")
	}
	if oneof.Fields().Len() == 0 {
		t.Fatal("the `body` oneof has no fields, so this test compares nothing")
	}

	for i := range oneof.Fields().Len() {
		field := oneof.Fields().Get(i)
		name := string(field.Name())
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// An envelope carrying an empty message of this body type. Empty is enough: what is under
			// test is the three switches' knowledge of the type, not any field inside it.
			env := &graphv1.EventEnvelope{EventId: "src:" + name}
			env.ProtoReflect().Set(field, protoreflect.ValueOfMessage(
				env.ProtoReflect().NewField(field).Message()))

			// 1. EventType names it, and names it exactly as the descriptor does — which is also exactly
			//    what the migrations' events_type_check constraint lists.
			got := EventType(env)
			if got == "" {
				t.Fatalf("EventType returned \"\" for a %s body, so the event cannot be stored at all "+
					"(log.events.type is NOT NULL and constrained)", name)
			}
			if got != name {
				t.Fatalf("EventType returned %q for the oneof field %q; the stored type must be the "+
					"field's own name, because that is the vocabulary events_type_check allows", got, name)
			}

			// 2. BodyMessage hands back something marshallable, which is what the append path stores.
			body := BodyMessage(env)
			if body == nil {
				t.Fatalf("BodyMessage returned nil for a %s body; the payload column would be empty and "+
					"the event would replay as a body with no content", name)
			}
			// protojson, because that is what the payload column holds and what setBody decodes.
			payload, err := protojson.Marshal(body)
			if err != nil {
				t.Fatalf("marshal the %s body: %v", name, err)
			}

			// 3. setBody reconstructs the SAME oneof case from the stored name and payload. This is the
			//    step `correlate_entity` was missing, and the one a replay depends on.
			read := &graphv1.EventEnvelope{EventId: env.GetEventId()}
			if err := setBody(read, got, payload); err != nil {
				t.Fatalf("setBody refused the type it was just told to store: %v. A type the reader does "+
					"not know is a type that cannot be replayed, which is the same thing as a type that "+
					"cannot be stored (constitution III)", err)
			}
			if EventType(read) != got {
				t.Errorf("a %s event read back as %q; the round trip changed the event's type",
					name, EventType(read))
			}
		})
	}
}
