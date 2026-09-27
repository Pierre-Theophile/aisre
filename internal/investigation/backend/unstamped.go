// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"fmt"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// errors_by_version on a pointer that carries no version stamp is answered here, by the engine,
// and never reaches a backend (005 FR-040b, ADR-0010 item 2).
//
// The term reads its split from Pointer.join_keys["version"]. A pointer without one used to be
// refused as outside the algebra, so no backend ever saw it, and "the logs carry no version" was
// indistinguishable from "the caller asked a malformed question". It is neither: it is a fact about
// the service's logs, and the published answer to a term whose data source the organisation does not
// have is NO_DATA naming the absent source (002 contract §1). Here the absent source is the stamp, and
// the answer names every convention that was searched for it.
//
// It is a function of the pointer alone. No vendor is asked, so there is no execution instant, no
// quota and no lag to report; the answer is byte-identical in live and recorded mode, needs no world
// entry — every recorded world's miss rate is unchanged — and is the same whichever backend owns the
// pointer. The discovery verdict, with each candidate's shares, lives with the feeder that looked
// (the pointer's node and the checkpoint); this answer names the list it was measured against.

// EngineBackendVersion is the backend version stamped on an answer the engine gives itself.
const EngineBackendVersion = "engine"

// UnstampedDataSource is the coverage data source of the engine's answer: what was looked at.
const UnstampedDataSource = "the pointer's version join key"

// IsUnstampedErrorsByVersion reports whether a term is an errors_by_version whose pointer names no
// version attribute — the case the engine answers.
func IsUnstampedErrorsByVersion(term *Term) bool {
	t, ok := term.GetTerm().(*investigationv1.AlgebraTerm_ErrorsByVersion)
	return ok && strings.TrimSpace(t.ErrorsByVersion.GetVersionAttribute()) == ""
}

// UnstampedAbsentSource is the absent-source statement: what is missing, and every convention the
// stamp was looked for under.
func UnstampedAbsentSource() string {
	// A noun phrase: the outcome renders it as "this organisation has no <absent source>".
	return fmt.Sprintf("version stamp on this service's logs (searched, in order, version stamp "+
		"conventions %s: %s; see docs/connectors/version-stamping.md for how to stamp them)",
		versionstamp.ConventionsVersion, strings.Join(versionstamp.ConventionLabels(), "; "))
}

// AnswerUnstamped builds the engine's NO_DATA for an unstamped errors_by_version. ok is false when the
// term is anything else, so a caller can try it first and fall through.
func AnswerUnstamped(req *Request, mode string) (resp *Response, ok bool, err error) {
	term := req.GetTerm()
	if !IsUnstampedErrorsByVersion(term) {
		return nil, false, nil
	}
	ebv := term.GetErrorsByVersion()
	if err := requirePointer(TermErrorsByVersion, ebv.GetPointer()); err != nil {
		return nil, false, err
	}
	if err := requireWindow(TermErrorsByVersion, "window", ebv.GetWindow()); err != nil {
		return nil, false, err
	}
	coverage := &investigationv1.Coverage{
		SearchedEntities:         []string{ebv.GetPointer().GetSelector()},
		DataSource:               UnstampedDataSource,
		WindowActuallyCovered:    ebv.GetWindow(),
		Sampling:                 "none",
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
	}
	resp, err = NewResponse(ResponseInput{
		Request:        req,
		Outcome:        NoData{Coverage: coverage, AbsentSource: UnstampedAbsentSource()},
		Mode:           mode,
		CostClass:      PublishedCostClass(TermErrorsByVersion),
		BackendVersion: EngineBackendVersion,
		Vocabulary:     ebv.GetPointer().GetVocabulary(),
	})
	if err != nil {
		return nil, false, err
	}
	return resp, true, nil
}
