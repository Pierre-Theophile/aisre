// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The recording boundary's own tests (T042, FR-137).

type bufferSink struct{ writes map[string][]byte }

func (s *bufferSink) Write(_ context.Context, name string, payload []byte) error {
	if s.writes == nil {
		s.writes = map[string][]byte{}
	}
	s.writes[name] = append([]byte(nil), payload...)
	return nil
}

func testSanitiser(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 3)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	return s
}

func jsonEncode(fields map[string]string) ([]byte, error) { return json.Marshal(fields) }

// There is no constructor that produces a recorder without a sanitiser, so there is no ordering in
// which a payload reaches a sink unsanitised.
func TestARecorderCannotBeBuiltWithoutASanitiser(t *testing.T) {
	if _, err := gcpfeeder.NewSanitisedRecorder(nil, &bufferSink{}); err == nil {
		t.Fatal("a recorder with no sanitiser was constructed; FR-137 puts sanitisation before disk")
	}
	if _, err := gcpfeeder.NewSanitisedRecorder(testSanitiser(t), nil); err == nil {
		t.Fatal("a recorder with no sink was constructed")
	}
	if _, err := gcpfeeder.NewSanitisedRecorder(testSanitiser(t), &bufferSink{}); err != nil {
		t.Fatalf("a valid recorder was refused: %v", err)
	}
}

// The ordinary path: an audit payload is pseudonymised, the principal is dropped, and what was
// dropped is on the manifest.
func TestARecordedPayloadIsPseudonymisedAndThePrincipalIsDropped(t *testing.T) {
	sink := &bufferSink{}
	recorder, err := gcpfeeder.NewSanitisedRecorder(testSanitiser(t), sink)
	if err != nil {
		t.Fatalf("NewSanitisedRecorder: %v", err)
	}
	err = recorder.Record(context.Background(), "gcp-audit-01", map[string]string{
		"protoPayload.authenticationInfo.principalEmail": "jane.doe@acme-corp.io",
		"protoPayload.methodName":                        "google.cloud.run.v2.Services.UpdateService",
		"protoPayload.resourceName":                      "projects/nova-production/locations/europe-west1/services/storefront",
		"region":                                         "europe-west1",
	}, jsonEncode)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	written := string(sink.writes["gcp-audit-01"])
	if written == "" {
		t.Fatal("nothing reached the sink")
	}
	for _, leaked := range []string{"jane.doe", "acme-corp", "nova-production", "storefront"} {
		if strings.Contains(written, leaked) {
			t.Fatalf("the recorded payload carries %q: %s", leaked, written)
		}
	}
	for _, kept := range []string{"europe-west1", "google.cloud.run.v2.Services.UpdateService", "px_res_"} {
		if !strings.Contains(written, kept) {
			t.Fatalf("the recorded payload lost %q, so the recording is not usable: %s", kept, written)
		}
	}
	if recorder.Written() != 1 {
		t.Fatalf("Written() is %d, want 1", recorder.Written())
	}
	if len(recorder.Dropped()) != 1 {
		t.Fatalf("the manifest lists %d drops, want 1 (the principal): %v", len(recorder.Dropped()), recorder.Dropped())
	}
	if recorder.PolicyVersion() != sanitise.PolicyVersion {
		t.Fatalf("the recorder reports policy version %q, want %q", recorder.PolicyVersion(), sanitise.PolicyVersion)
	}
}

// An unassigned field drops the whole payload rather than writing part of it: FR-140's "drop rather
// than promise". A fixture is never committed on the promise of being cleaned later.
func TestAPayloadWithAnUnassignedFieldIsDroppedRatherThanPartlyWritten(t *testing.T) {
	sink := &bufferSink{}
	recorder, err := gcpfeeder.NewSanitisedRecorder(testSanitiser(t), sink)
	if err != nil {
		t.Fatalf("NewSanitisedRecorder: %v", err)
	}
	err = recorder.Record(context.Background(), "gcp-audit-02", map[string]string{
		"region":                       "europe-west1",
		"protoPayload.newFieldFromGCP": "something nobody thought about",
	}, jsonEncode)
	if err == nil {
		t.Fatal("a payload with an unassigned field was written")
	}
	var unassigned *sanitise.UnassignedError
	if !errors.As(err, &unassigned) {
		t.Fatalf("the refusal is %T, not *UnassignedError: %v", err, err)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("%d payloads reached the sink from a refused record", len(sink.writes))
	}
	if recorder.Written() != 0 {
		t.Fatalf("Written() is %d after a refusal", recorder.Written())
	}
}

// The assertion is on the bytes the sink receives, not on the map — so an encoder that helpfully
// added a field the field walk never saw is still caught. This is the case a check on the map would
// miss, and the reason RecordBytes exists.
func TestTheAssertionIsOnTheBytesTheSinkReceivesNotOnTheMap(t *testing.T) {
	sink := &bufferSink{}
	recorder, err := gcpfeeder.NewSanitisedRecorder(testSanitiser(t), sink)
	if err != nil {
		t.Fatalf("NewSanitisedRecorder: %v", err)
	}
	// An encoder that appends a principal of its own: a request-id header, an audit trailer, a
	// well-meant "recorded by" line. The field walk saw nothing wrong.
	helpful := func(fields map[string]string) ([]byte, error) {
		out, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		return append(out, []byte(`{"recordedBy":"jane.doe@acme-corp.io"}`)...), nil
	}
	err = recorder.Record(context.Background(), "gcp-audit-03", map[string]string{"region": "europe-west1"}, helpful)
	if err == nil {
		t.Fatal("an encoder that added a principal after the field walk wrote to the sink")
	}
	if len(sink.writes) != 0 {
		t.Fatalf("%d payloads reached the sink", len(sink.writes))
	}
}

// A canary that reached the bytes fails the write, before the file exists.
func TestACanaryInTheBytesRefusesTheWriteBeforeTheFileExists(t *testing.T) {
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 3)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	set := sanitise.NewCanarySet(key)
	seeded, err := set.Seed("the nova-production audit log", sanitise.CanaryInfrastructure)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	sink := &bufferSink{}
	recorder, err := gcpfeeder.NewSanitisedRecorder(s.WithCanaries(set), sink)
	if err != nil {
		t.Fatalf("NewSanitisedRecorder: %v", err)
	}
	err = recorder.RecordBytes(context.Background(), "gcp-audit-04",
		[]byte(`{"service":"`+seeded[0].Token+`"}`))
	if err == nil {
		t.Fatal("a payload carrying a raw canary was written")
	}
	var survived *sanitise.CanarySurvivedError
	if !errors.As(err, &survived) {
		t.Fatalf("the refusal is %T, not *CanarySurvivedError: %v", err, err)
	}
	if len(sink.writes) != 0 {
		t.Fatal("the canary payload reached the sink")
	}
	// A clean payload still writes, so the gate is not simply always red.
	if err := recorder.RecordBytes(context.Background(), "gcp-audit-05", []byte(`{"region":"europe-west1"}`)); err != nil {
		t.Fatalf("a clean payload was refused: %v", err)
	}
}
