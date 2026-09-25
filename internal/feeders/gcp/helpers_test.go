// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// propsMap renders a Props builder as `key -> string` so a test can assert on what a reader of a
// golden would actually see.
//
// It renders through the builder's own Build, which runs the graph's validator: a test that read the
// builder's internals would pass on properties the graph refuses.
func propsMap(t *testing.T, props *feeder.Props) map[string]string {
	t.Helper()
	built, err := props.Build()
	if err != nil {
		t.Fatalf("Props.Build: %v", err)
	}
	out := map[string]string{}
	for key, value := range built.GetFields() {
		out[key] = renderValue(value)
	}
	return out
}

func renderValue(v *structpb.Value) string {
	switch kind := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return kind.StringValue
	case *structpb.Value_BoolValue:
		return fmt.Sprintf("%t", kind.BoolValue)
	case *structpb.Value_NumberValue:
		return fmt.Sprintf("%g", kind.NumberValue)
	case *structpb.Value_ListValue:
		parts := make([]string, 0, len(kind.ListValue.GetValues()))
		for _, item := range kind.ListValue.GetValues() {
			parts = append(parts, renderValue(item))
		}
		return fmt.Sprintf("%v", parts)
	default:
		return v.String()
	}
}
