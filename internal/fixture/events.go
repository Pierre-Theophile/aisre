// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Reading an events.jsonl (contracts/fixture-format.md §"Canonical serialization").
//
// Each line is one EventEnvelope in canonical proto JSON plus two fields the log added and the
// schema does not have: `observedAt`, the observed-time lower bound assigned at append, and
// `appendedSeq`, the physical log order starting at 1. They are lifted off before the envelope
// is unmarshalled.
//
// Unmarshalling is strict (DiscardUnknown: false) on purpose. A fixture line carrying a field
// the published schema does not have is a broken fixture, and accepting it silently would let
// the recorded event stream drift away from the contract that connector authors write against
// (constitution IX, FR-049).

// observedAtField and appendedSeqField are the two log-added keys stripped from every line.
const (
	observedAtField  = "observedAt"
	appendedSeqField = "appendedSeq"
)

// Event is one line of a fixture's events.jsonl: the envelope as the source sent it, plus what
// the log recorded about it.
type Event struct {
	// Envelope is the event exactly as the schema defines it.
	Envelope *graphv1.EventEnvelope
	// ObservedAt is the observed time the log assigned when the event was first accepted.
	// Replay applies the event with this value, never with the replay clock (FR-023).
	ObservedAt time.Time
	// AppendedSeq is the line's physical log position, starting at 1.
	AppendedSeq int64
	// Principal is the authenticated individual behind the event. It is empty for everything a
	// feeder emits and set only for the human decisions the manifest declares (FR-041).
	Principal string
}

// ReadEvents reads a fixture's accepted-event file.
//
// Every line must carry `observedAt`: an accepted event has one by definition, and replaying
// without it would stamp the replay clock onto the projection and break FR-023 silently.
func ReadEvents(path string) ([]Event, error) {
	return readEventFile(path, true)
}

// ReadRejected reads a fixture's must-be-rejected event file.
//
// `observedAt` is optional here: the log never accepted these events, so it never assigned one
// (fixtures/README.md §"Extension: rejected.jsonl"). A caller submitting them uses the
// envelope's own source_observed_at, or the current clock.
func ReadRejected(path string) ([]Event, error) {
	return readEventFile(path, false)
}

func readEventFile(path string, requireObservedAt bool) ([]Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixture: read events: %w", err)
	}

	var events []Event
	for i, line := range strings.Split(string(raw), "\n") {
		lineNo := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		event, err := parseEventLine(line, int64(len(events)+1), requireObservedAt)
		if err != nil {
			return nil, fmt.Errorf("fixture: %s line %d: %w", path, lineNo, err)
		}
		events = append(events, event)
	}
	return events, nil
}

func parseEventLine(line string, seq int64, requireObservedAt bool) (Event, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return Event{}, fmt.Errorf("not a JSON object: %w", err)
	}

	event := Event{AppendedSeq: seq}
	if encoded, ok := fields[observedAtField]; ok {
		var text string
		if err := json.Unmarshal(encoded, &text); err != nil {
			return Event{}, fmt.Errorf("%s: %w", observedAtField, err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return Event{}, fmt.Errorf("%s: %w", observedAtField, err)
		}
		event.ObservedAt = parsed.UTC()
	} else if requireObservedAt {
		return Event{}, fmt.Errorf("no %s: an accepted event carries the observed time the log assigned it (FR-023)", observedAtField)
	}
	if encoded, ok := fields[appendedSeqField]; ok {
		var recorded int64
		if err := json.Unmarshal(encoded, &recorded); err != nil {
			return Event{}, fmt.Errorf("%s: %w", appendedSeqField, err)
		}
		if recorded != seq {
			return Event{}, fmt.Errorf("%s is %d but this is event %d: the field is the line number (contracts/fixture-format.md)",
				appendedSeqField, recorded, seq)
		}
	}
	delete(fields, observedAtField)
	delete(fields, appendedSeqField)

	body, err := json.Marshal(fields)
	if err != nil {
		return Event{}, err
	}
	envelope := &graphv1.EventEnvelope{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, envelope); err != nil {
		return Event{}, fmt.Errorf("does not match the published event schema: %w", err)
	}
	event.Envelope = envelope
	return event, nil
}
