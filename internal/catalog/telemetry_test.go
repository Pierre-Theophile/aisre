// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Pierre-Theophile/aisre/internal/catalog"
)

// The attributor's telemetry (009 T038): decisions by layer, and the classifier's fallbacks.
func TestTheAttributorCountsItsDecisions(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	w := newWorld(t, catalog.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		catalog.WithClassifier(&stubClassifier{err: errors.New("no credential")}, 0))
	w.entry("checkout", "Checkout", "confirmed")
	w.service("checkout-api", "production")
	w.alert("1", "checkout-api", "production")
	w.service("zeta-gateway", "production")
	w.alert("2", "zeta-gateway", "production")
	w.sweep()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				layer, _ := dp.Attributes.Value(attribute.Key("layer"))
				got[m.Name+"/"+layer.AsString()] += dp.Value
			}
		}
	}
	for key, want := range map[string]int64{
		catalog.MetricDecisions + "/probable":  1, // checkout-api is Checkout by P1
		catalog.MetricDecisions + "/unknown":   1,
		catalog.MetricClassifierFallback + "/": 1,
	} {
		if got[key] != want {
			t.Errorf("%s = %d, want %d (all: %v)", key, got[key], want, got)
		}
	}
}
