// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Unit tests for the parts of the aggregator that decide what a span means (T051). What it
// does with a whole recording is tested against the corpora in feeder_test.go; these are the
// decisions that are easier to see one at a time.

func TestResolveCallee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attrs    map[string]string
		wantNS   string
		wantVal  string
		wantShrt string
		external bool
		none     bool
	}{
		{
			name:     "a peer service is a service-to-service call",
			attrs:    map[string]string{attrPeerService: "checkout"},
			wantNS:   feeder.NSOTelService,
			wantVal:  "checkout",
			wantShrt: "checkout",
		},
		{
			name: "a peer service with an address is a dependency under its peer name",
			attrs: map[string]string{
				attrPeerService:          "redis",
				feeder.AttrDBSystem:      "redis",
				feeder.AttrServerAddress: "redis.shop.svc.cluster.local",
				feeder.AttrServerPort:    "6379",
			},
			wantNS:   feeder.NSOTelService,
			wantVal:  "redis",
			wantShrt: "redis",
			external: true,
		},
		{
			name: "a database is addressed by its host",
			attrs: map[string]string{
				feeder.AttrDBSystem:      "postgresql",
				feeder.AttrServerAddress: "payments-db.shop.svc.cluster.local",
			},
			wantNS:   feeder.NSServerAddress,
			wantVal:  "payments-db.shop.svc.cluster.local",
			wantShrt: "payments-db",
			external: true,
		},
		{
			name: "the 1.30 spelling of db.system is read too",
			attrs: map[string]string{
				feeder.AttrDBSystemName:  "mysql",
				feeder.AttrServerAddress: "orders-db.shop.svc.cluster.local",
			},
			wantNS:   feeder.NSServerAddress,
			wantVal:  "orders-db.shop.svc.cluster.local",
			wantShrt: "orders-db",
			external: true,
		},
		{
			name: "an https host with no peer service is an external dependency",
			attrs: map[string]string{
				feeder.AttrServerAddress: "api.stripe.com",
				feeder.AttrURLScheme:     "https",
			},
			wantNS:   feeder.NSServerAddress,
			wantVal:  "api.stripe.com",
			wantShrt: "stripe",
			external: true,
		},
		{
			name:  "an address with neither a scheme nor a database system says nothing",
			attrs: map[string]string{feeder.AttrServerAddress: "10.0.0.7"},
			none:  true,
		},
		{
			name:  "an internal span is not a call",
			attrs: map[string]string{},
			none:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := resolveCallee(tc.attrs)
			if ok == tc.none {
				t.Fatalf("resolveCallee(%v) ok = %v, want %v", tc.attrs, ok, !tc.none)
			}
			if tc.none {
				return
			}
			if got.key.ns != tc.wantNS || got.key.value != tc.wantVal {
				t.Errorf("ref = %s=%s, want %s=%s", got.key.ns, got.key.value, tc.wantNS, tc.wantVal)
			}
			if got.short != tc.wantShrt {
				t.Errorf("short name = %q, want %q", got.short, tc.wantShrt)
			}
			if got.external != tc.external {
				t.Errorf("external = %v, want %v", got.external, tc.external)
			}
		})
	}
}

func TestShortHostName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"payments-db.shop.svc.cluster.local": "payments-db",
		"redis.shop.svc.cluster.local":       "redis",
		"api.stripe.com":                     "stripe",
		"stripe.com":                         "stripe",
		"queue.eu-west-1.amazonaws.com":      "amazonaws",
		"localhost":                          "localhost",
		"10.0.0.7":                           "10.0.0.7",
		"db.internal":                        "db",
		"API.Stripe.COM.":                    "stripe",
		"cache:6379":                         "cache",
		"":                                   "",
	}
	for host, want := range cases {
		if got := shortHostName(host); got != want {
			t.Errorf("shortHostName(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestSpanWeightScalesBySamplingProbability(t *testing.T) {
	t.Parallel()
	cases := map[string]float64{
		"":     1,
		"0.1":  10,
		"1":    1,
		"0.01": 100,
		// Nonsense is not a licence to invent traffic: the span counts once.
		"0":    1,
		"-0.5": 1,
		"2":    1,
		"many": 1,
	}
	for raw, want := range cases {
		attrs := map[string]string{}
		if raw != "" {
			attrs[attrSamplingProbability] = raw
		}
		if got := spanWeight(attrs); got != want {
			t.Errorf("spanWeight(sampling.probability=%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestReadAttrsKeepsOnlyWhatIsWhitelisted(t *testing.T) {
	t.Parallel()
	attrs := []*commonpb.KeyValue{
		str("peer.service", "checkout"),
		str("db.statement", "SELECT * FROM orders WHERE id = 42"),
		str("http.url", "https://shop.example/checkout?token=secret"),
		str("exception.stacktrace", "goroutine 1 [running]:"),
		{Key: "server.port", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 5432}}},
		{Key: "latency_samples", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
			ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
				{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 12.1}},
				{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 18.4}},
			}},
		}}},
	}
	got := readAttrs(attrs, allowedSpanAttrs)
	want := map[string]string{"peer.service": "checkout", "server.port": "5432"}
	if len(got) != len(want) {
		t.Fatalf("readAttrs kept %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("readAttrs[%q] = %q, want %q", key, got[key], value)
		}
	}
}

// TestArrayAttributesAreNotScalars is the guarantee that makes the whitelist enough: even a
// whitelisted key cannot carry an array into a property, because an array is the shape
// telemetry arrives in.
func TestArrayAttributesAreNotScalars(t *testing.T) {
	t.Parallel()
	array := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}}
	if _, ok := scalarValue(array); ok {
		t.Error("an array attribute was read as a scalar")
	}
	nested := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{}}}
	if _, ok := scalarValue(nested); ok {
		t.Error("a nested map attribute was read as a scalar")
	}
}

// TestWindowsCloseOnlyAfterTheReorderingWindow is what makes a permutation inside the declared
// window safe: a window stays open until the watermark is past its end by a full window.
func TestWindowsCloseOnlyAfterTheReorderingWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	agg := NewAggregator(5*time.Minute, 5*time.Minute, nil)
	if w := agg.windowFor(start.Add(time.Minute)); w == nil || !w.start.Equal(start) {
		t.Fatalf("window for 13:01 = %v, want one starting at 13:00", w)
	}

	agg.Advance(start.Add(9 * time.Minute))
	if closed := agg.Closed(); len(closed) != 0 {
		t.Fatalf("the 13:00 window closed at 13:09, %d windows", len(closed))
	}
	agg.Advance(start.Add(10 * time.Minute))
	if closed := agg.Closed(); len(closed) != 0 {
		t.Fatalf("the 13:00 window closed exactly at its end plus the reordering window")
	}
	agg.Advance(start.Add(10*time.Minute + time.Second))
	closed := agg.Closed()
	if len(closed) != 1 || !closed[0].start.Equal(start) {
		t.Fatalf("closed = %v, want the 13:00 window", closed)
	}

	// A span for a window that has already closed is dropped, loudly, rather than being
	// attributed to a window it does not belong to.
	if w := agg.windowFor(start.Add(time.Minute)); w != nil {
		t.Error("a span was accepted into a window that had already closed")
	}
}

// TestClosedWindowsAreChronological: emission order across windows is time order, whatever
// order the payloads arrived in.
func TestClosedWindowsAreChronological(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	agg := NewAggregator(5*time.Minute, 0, nil)
	for _, offset := range []time.Duration{10, 0, 5} {
		agg.windowFor(start.Add(offset * time.Minute))
	}
	agg.Advance(start.Add(time.Hour))
	closed := agg.Closed()
	if len(closed) != 3 {
		t.Fatalf("closed %d windows, want 3", len(closed))
	}
	for i, want := range []time.Duration{0, 5, 10} {
		if !closed[i].start.Equal(start.Add(want * time.Minute)) {
			t.Errorf("window %d starts %s, want %s", i, closed[i].start, start.Add(want*time.Minute))
		}
	}
}

func TestKeepMaxIsOrderIndependent(t *testing.T) {
	t.Parallel()
	if keepMax(keepMax("", "0.9.7"), "0.9.8") != keepMax(keepMax("", "0.9.8"), "0.9.7") {
		t.Error("the attribute a window keeps depends on which span arrived first")
	}
}

func str(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: value},
	}}
}
