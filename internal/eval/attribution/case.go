// SPDX-License-Identifier: Apache-2.0

package attribution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/catalog/classify"
	"github.com/Pierre-Theophile/aisre/internal/embedding"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// LabelsFile is the file in a case directory that says what each signal is about.
const LabelsFile = "labels.yaml"

// ClassifierFile turns layer 3 on for a case: the embedding classifier with the calibration it
// names, over the vectors recorded beside it (embedding.VectorsFile), so a replay needs no
// provider (FR-036).
const ClassifierFile = "classifier.yaml"

// Case is one labelled attribution case (research R13): a recorded stream, and what each of its
// signals is about.
type Case struct {
	ID       string
	Dir      string
	Manifest *fixture.Manifest
	// Feed is the recorded stream without the attributor's decisions: what a replay of the case
	// runs the attributor over.
	Feed []fixture.Event
	// Recorded is the attributor's decisions as events.jsonl holds them.
	Recorded []fixture.Event
	Labels   []Label
	// Calibration is the case's classifier calibration, nil when the case runs without layer 3.
	Calibration *classify.Calibration
	// Embedder is what the classifier embeds through: the case's recorded vectors, unless a
	// recording run replaces it with a live model.
	Embedder knowledge.Embedder
}

// Label is one signal and the entries it is about; none is `unknown`, the right answer when the
// evidence does not decide.
type Label struct {
	Signal string   `yaml:"signal" json:"signal"`
	Expect []string `yaml:"expect" json:"expect"`
}

// Load reads the case in dir.
func Load(dir string) (*Case, error) {
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	recorded, err := fixture.ReadEvents(m.EventsPath())
	if err != nil {
		return nil, err
	}
	c := &Case{ID: m.ID, Dir: dir, Manifest: m}
	for _, e := range recorded {
		if e.Envelope.GetSourceId() == graph.AttributorSourceID {
			c.Recorded = append(c.Recorded, e)
		} else {
			c.Feed = append(c.Feed, e)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, LabelsFile))
	if err != nil {
		return nil, err
	}
	var labels struct {
		Labels []Label `yaml:"labels"`
	}
	if err := yaml.Unmarshal(raw, &labels); err != nil {
		return nil, fmt.Errorf("attribution: %s: %w", filepath.Join(dir, LabelsFile), err)
	}
	if len(labels.Labels) == 0 {
		return nil, fmt.Errorf("attribution: %s labels nothing", filepath.Join(dir, LabelsFile))
	}
	for _, l := range labels.Labels {
		if _, err := graph.ParseRef(l.Signal); err != nil {
			return nil, fmt.Errorf("attribution: %s: label %q: %w", c.ID, l.Signal, err)
		}
	}
	c.Labels = labels.Labels
	if raw, err := os.ReadFile(filepath.Join(dir, ClassifierFile)); err == nil {
		var spec struct {
			Calibration *classify.Calibration `yaml:"calibration"`
		}
		if err := yaml.Unmarshal(raw, &spec); err != nil || spec.Calibration == nil || spec.Calibration.Slope <= 0 {
			return nil, fmt.Errorf("attribution: %s: want a calibration with a positive slope: %w", filepath.Join(dir, ClassifierFile), err)
		}
		c.Calibration = spec.Calibration
		vectors := filepath.Join(dir, embedding.VectorsFile)
		if _, err := os.Stat(vectors); err == nil {
			if c.Embedder, err = embedding.LoadRecorded(vectors); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return c, nil
}

// LoadAll reads every case named: a directory that is a case, or that holds cases one level down.
func LoadAll(dirs ...string) ([]*Case, error) {
	var out []*Case
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, LabelsFile)); err == nil {
			c, err := Load(dir)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
			continue
		}
		found, err := filepath.Glob(filepath.Join(dir, "*", LabelsFile))
		if err != nil {
			return nil, err
		}
		sort.Strings(found)
		for _, labels := range found {
			c, err := Load(filepath.Dir(labels))
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("attribution: no labelled case (a directory with " + LabelsFile + ") under the given directories")
	}
	return out, nil
}

// Replay applies the case's feed, with its human decisions, to an empty store and runs the
// attributor after every event, as serve does. It returns the projector and the decisions the
// attributor took, in log order.
//
// Decisions are stamped one reordering window after the event, the widest any source declares: a
// replay may deliver any feeder event up to its window late (fixture verify's shuffles do), and a
// decision must never arrive before the signal it is about.
func Replay(ctx context.Context, store *postgres.Store, c *Case, opts ...catalog.Option) (*projector.Projector, []fixture.Event, error) {
	m := c.Manifest
	stream, err := fixture.WithHumanDecisions(m, c.Feed)
	if err != nil {
		return nil, nil, fmt.Errorf("attribution: %s: human decisions: %w", c.ID, err)
	}
	p := projector.New(store)
	sources := []eventlog.Source{{SourceID: fixture.HumanSourceID, Kind: "human", Ordering: "none", SchemaVersion: m.SchemaVersion}}
	for _, src := range m.Sources {
		if src.SourceID == graph.AttributorSourceID || src.SourceID == fixture.HumanSourceID {
			continue
		}
		sources = append(sources, eventlog.Source{SourceID: src.SourceID, Kind: src.Kind, Ordering: src.Ordering,
			ReorderingWindow: src.ReorderingWindow, SchemaVersion: m.SchemaVersion})
	}
	for _, src := range sources {
		if err := p.RegisterSource(ctx, src); err != nil {
			return nil, nil, fmt.Errorf("attribution: %s: register %s: %w", c.ID, src.SourceID, err)
		}
	}

	settle := time.Second
	for _, src := range m.Sources {
		settle = max(settle, src.ReorderingWindow+time.Second)
	}
	var clock time.Time
	if c.Calibration != nil {
		if c.Embedder == nil {
			return nil, nil, fmt.Errorf("attribution: %s has a classifier and no recorded %s; record them with `aisre eval attribution --record-vectors`",
				c.ID, embedding.VectorsFile)
		}
		opts = append(opts, catalog.WithClassifier(classify.NewEmbedding(c.Embedder, *c.Calibration), time.Minute))
	}
	opts = append([]catalog.Option{catalog.WithClock(func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	})}, opts...)
	a := catalog.New(p, opts...)
	if err := a.Register(ctx); err != nil {
		return nil, nil, fmt.Errorf("attribution: %s: register the attributor: %w", c.ID, err)
	}
	for _, e := range stream {
		r, err := p.ApplyWithOptions(ctx, e.Envelope, projector.ApplyOptions{ObservedAt: e.ObservedAt, Principal: e.Principal})
		if err != nil {
			return nil, nil, fmt.Errorf("attribution: %s: apply %s: %w", c.ID, e.Envelope.GetEventId(), err)
		}
		if r.GetStatus() == graphv1.IngestResult_REJECTED {
			return nil, nil, fmt.Errorf("attribution: %s: apply %s: %s %s", c.ID, e.Envelope.GetEventId(), r.GetReasonCode(), r.GetReasonDetail())
		}
		if at := e.ObservedAt.Add(settle); at.After(clock) {
			clock = at
		}
		if _, err := a.Sweep(ctx); err != nil {
			return nil, nil, fmt.Errorf("attribution: %s: sweep after %s: %w", c.ID, e.Envelope.GetEventId(), err)
		}
	}
	var decided []fixture.Event
	if err := p.Log().BySource(ctx, graph.AttributorSourceID, 0, func(r *eventlog.Record) error {
		decided = append(decided, fixture.Event{Envelope: r.Envelope, ObservedAt: r.ObservedAt.UTC()})
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("attribution: %s: read decisions: %w", c.ID, err)
	}
	return p, decided, nil
}
