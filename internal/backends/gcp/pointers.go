// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Accepting the feeders' pointers without translation (T108, FR-111).
//
// The rule has two halves and the second is the one that matters. A pointer this backend **can**
// execute is executed **as minted** — no rewriting, no normalisation, no "equivalent" filter
// composed from its attributes. A pointer it **cannot** execute is reported
// `QUERY_FAILED / UNSUPPORTED_POINTER` **naming the kind and the vocabulary**, and is never
// executed as something else.
//
// Executing it as something else is the failure worth spelling out: a `promql` selector or an
// `otel-semconv` attribute set could each be turned into *a* Monitoring filter that returns *a*
// number, and that number would be an answer to a question nobody asked, carrying a coverage block
// that looked exactly as trustworthy as a real one. A refusal costs an investigation one term. A
// confident wrong number costs it the conclusion.

// termPointer returns the pointer a term carries and the pointer kind that term needs. A term that
// carries no pointer — the handle-bearing ones, and `error_spans`, which names two entity ids —
// reports ok=false and is not checked here.
func termPointer(term *engine.Term) (pointer *graphv1.Pointer, want graphv1.PointerKind, ok bool) {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return t.Compare.GetPointer(), graphv1.PointerKind_METRIC, true
	case *investigationv1.AlgebraTerm_Onset:
		return t.Onset.GetPointer(), graphv1.PointerKind_METRIC, true
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return t.ErrorsByVersion.GetPointer(), graphv1.PointerKind_METRIC, true
	case *investigationv1.AlgebraTerm_MonitorState:
		// A monitor-state pointer says which resource's alerts to read, and the feeder mints
		// that as a metric pointer: the alert watches a metric, and the resource labels in the
		// selector are how the incidents are matched to the entity.
		return t.MonitorState.GetPointer(), graphv1.PointerKind_METRIC, true
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return t.NewLogPatterns.GetPointer(), graphv1.PointerKind_LOG, true
	default:
		return nil, graphv1.PointerKind_POINTER_KIND_UNSPECIFIED, false
	}
}

// executableVocabulary is the vocabulary this backend executes for each pointer kind. It is a
// closed mapping rather than a check for "one of ours": a LOG pointer written in the Monitoring
// filter language is not a log pointer with the wrong label, it is a query for a different API.
func executableVocabulary(kind graphv1.PointerKind) (string, bool) {
	switch kind {
	case graphv1.PointerKind_METRIC:
		return feeder.VocabGCPMonitoringFilter, true
	case graphv1.PointerKind_LOG:
		return feeder.VocabGCPLoggingQuery, true
	default:
		return "", false
	}
}

// checkPointer refuses a pointer this backend cannot execute, naming the kind and the vocabulary.
// It returns refused=false for a term that carries an executable pointer or none at all.
func (b *Backend) checkPointer(req *engine.Request, name string) (*engine.Response, bool, error) {
	pointer, want, ok := termPointer(req.GetTerm())
	if !ok {
		return nil, false, nil
	}
	if pointer == nil {
		resp, err := b.refusePointer(req, fmt.Sprintf(
			"gcp: %s carries no pointer; a telemetry term says where to look and this one says "+
				"nothing, so there is nothing to execute", name))
		return resp, true, err
	}

	wantVocab, executable := executableVocabulary(pointer.GetKind())
	if !executable {
		resp, err := b.refusePointer(req, fmt.Sprintf(
			"gcp: %s was given a %s pointer in vocabulary %q; this backend executes METRIC "+
				"pointers in %s and LOG pointers in %s, and a pointer of another kind is refused "+
				"rather than executed as something else",
			name, pointer.GetKind(), pointer.GetVocabulary(),
			feeder.VocabGCPMonitoringFilter, feeder.VocabGCPLoggingQuery))
		return resp, true, err
	}
	if pointer.GetKind() != want {
		resp, err := b.refusePointer(req, fmt.Sprintf(
			"gcp: %s needs a %s pointer and was given a %s pointer in vocabulary %q",
			name, want, pointer.GetKind(), pointer.GetVocabulary()))
		return resp, true, err
	}
	if pointer.GetVocabulary() != wantVocab {
		// Named in full, including what this backend does execute, because the caller reading
		// this is usually holding a pointer minted for a different backend entirely — a
		// `promql` or `datadog-query` selector — and the useful sentence is which vocabulary
		// this one speaks, not that theirs is wrong.
		resp, err := b.refusePointer(req, fmt.Sprintf(
			"gcp: %s was given a %s pointer in vocabulary %q; this backend executes %q for %s "+
				"pointers. The selector is executed as minted, so a pointer in another "+
				"vocabulary is refused rather than translated — a selector translated and back "+
				"is a selector that can silently stop matching",
			name, pointer.GetKind(), pointer.GetVocabulary(), wantVocab, pointer.GetKind()))
		return resp, true, err
	}
	if pointer.GetSelector() == "" {
		resp, err := b.refusePointer(req, fmt.Sprintf(
			"gcp: %s carries a %s pointer in %q with an empty selector", name, pointer.GetKind(), wantVocab))
		return resp, true, err
	}
	return nil, false, nil
}

// refusePointer is the typed refusal FR-111 requires.
func (b *Backend) refusePointer(req *engine.Request, detail string) (*engine.Response, error) {
	return b.refuse(req, investigationv1.FailureReason_UNSUPPORTED_POINTER, detail)
}
