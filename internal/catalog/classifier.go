// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/attribution"
)

// Layer 3 in the attributor (feature 009 T034, research R6, FR-034–FR-036): off unless the model
// configuration names a classifier. It never blocks a decision: an error, a timeout or a missing
// credential leaves the decision to layers 0–2 and the evidence says why.

// DefaultClassifierTimeout bounds one classifier call (research R6).
const DefaultClassifierTimeout = 2 * time.Second

// WithClassifier turns layer 3 on with c, each call bounded by timeout (DefaultClassifierTimeout
// when zero).
func WithClassifier(c attribution.Classifier, timeout time.Duration) Option {
	return func(a *Attributor) {
		if timeout <= 0 {
			timeout = DefaultClassifierTimeout
		}
		a.classifier, a.classifierTimeout = c, timeout
	}
}

// classify asks the classifier about a signal layers 0–2 left `unknown`.
func (a *Attributor) classify(ctx context.Context, signal Signal, facts Facts, d Decision) (Decision, error) {
	profiles := profilesOf(facts)
	if len(profiles) == 0 {
		return d, nil
	}
	call, cancel := context.WithTimeout(ctx, a.classifierTimeout)
	defer cancel()
	call, span := a.telemetry.tracer.Start(call, "catalog.attributor.classify",
		trace.WithAttributes(attribute.String("sre.attribution.classifier", a.classifier.ID()), attribute.Int("sre.attribution.entries", len(profiles))))
	started := time.Now()
	scored, err := a.classifier.Classify(call, classifierSignal(signal), profiles)
	a.telemetry.classified(ctx, a.classifier.ID(), time.Since(started), err != nil && ctx.Err() == nil)
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	if err != nil {
		if ctx.Err() != nil {
			// The attributor itself is stopping: no decision, rather than one recording a fallback
			// that never happened.
			return Decision{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("the classifier did not answer within " + a.classifierTimeout.String())
		}
		a.logger.Warn("attribution classifier failed; layers 0-2 decide", "classifier", a.classifier.ID(), "err", err)
		return WithClassifierFailure(d, a.classifier.ID(), err), nil
	}
	out := make([]Classified, 0, len(scored))
	for _, s := range scored {
		out = append(out, Classified{Slug: s.Slug, Score: s.Score, Rationale: s.Rationale})
	}
	return a.rules.WithClassifier(d, a.classifier.ID(), out, facts.Entries, facts.Hidden), nil
}

// profilesOf describes every entry a signal may be attributed to, sorted by slug.
func profilesOf(facts Facts) []attribution.EntryProfile {
	aliases := map[string][]attribution.Name{}
	for _, al := range facts.Aliases {
		aliases[al.Slug] = append(aliases[al.Slug], publicName(al.Name))
	}
	slugs := make([]string, 0, len(facts.Entries))
	for slug, e := range facts.Entries {
		if slug == graph.HiddenEntrySlug || facts.Hidden[slug] || e.Status == graph.CatalogStatusHidden {
			continue
		}
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)
	out := make([]attribution.EntryProfile, 0, len(slugs))
	for _, slug := range slugs {
		e := facts.Entries[slug]
		names := aliases[slug]
		slices.SortFunc(names, func(a, b attribution.Name) int { return strings.Compare(a.String(), b.String()) })
		out = append(out, attribution.EntryProfile{Slug: slug, DisplayName: e.Display, Description: e.Description, Aliases: names})
	}
	return out
}

func classifierSignal(s Signal) attribution.Signal {
	out := attribution.Signal{Ref: publicName(Name{Ref: s.Ref}), Kind: s.Kind, Text: s.Text}
	for _, n := range s.Names {
		out.Names = append(out.Names, publicName(n))
	}
	return out
}

func publicName(n Name) attribution.Name {
	return attribution.Name{Namespace: n.Ref.Namespace, Value: n.Ref.Value, Environment: n.Environment}
}
