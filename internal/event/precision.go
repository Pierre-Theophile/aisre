// SPDX-License-Identifier: Apache-2.0

package event

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// StorePrecision returns env with every timestamp in it, envelope and body, truncated to the
// microsecond. env itself is not modified; it is returned unchanged when nothing needed cutting.
//
// The graph stores time in PostgreSQL, which keeps microseconds, while a feeder stamps events
// with Go's clock, which has nanoseconds. An instant the projector reasons about in nanoseconds
// and the database then rounds is two different instants: two assertions 400 ns apart cut the
// valid timeline into a segment that is empty once stored, and the insert is refused. Cutting
// every instant to what the store keeps, once, before the log and the projector see it, makes
// the instant the projector plans with the instant it reads back, and a replay from the log
// equal to the first application. pgx truncates when it encodes, so this agrees with it.
func StorePrecision(env *graphv1.EventEnvelope) *graphv1.EventEnvelope {
	if env == nil || !hasSubMicrosecond(env.ProtoReflect()) {
		return env
	}
	out := proto.Clone(env).(*graphv1.EventEnvelope)
	truncate(out.ProtoReflect())
	return out
}

var timestampName = (&timestamppb.Timestamp{}).ProtoReflect().Descriptor().FullName()

func hasSubMicrosecond(m protoreflect.Message) bool {
	found := false
	walkTimestamps(m, func(ts protoreflect.Message) {
		if ts.Get(ts.Descriptor().Fields().ByName("nanos")).Int()%1000 != 0 {
			found = true
		}
	})
	return found
}

func truncate(m protoreflect.Message) {
	walkTimestamps(m, func(ts protoreflect.Message) {
		nanos := ts.Descriptor().Fields().ByName("nanos")
		n := ts.Get(nanos).Int()
		ts.Set(nanos, protoreflect.ValueOfInt32(int32(n-n%1000)))
	})
}

// walkTimestamps calls visit on every google.protobuf.Timestamp reachable from m, in singular,
// repeated and map fields alike.
func walkTimestamps(m protoreflect.Message, visit func(protoreflect.Message)) {
	if m.Descriptor().FullName() == timestampName {
		visit(m)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			list := v.List()
			for i := range list.Len() {
				walkTimestamps(list.Get(i).Message(), visit)
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				walkTimestamps(mv.Message(), visit)
				return true
			})
		case fd.Message() != nil && !fd.IsList() && !fd.IsMap():
			walkTimestamps(v.Message(), visit)
		}
		return true
	})
}
