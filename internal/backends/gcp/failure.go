// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Turning a vendor error into one of the published failure reasons.
//
// The published set is closed — REJECTED_BY_BACKEND, NOT_PERMITTED, RATE_LIMITED, TIMED_OUT,
// UNSUPPORTED_POINTER, OUTSIDE_ALGEBRA, OUTSIDE_RETENTION — and the mapping matters because an
// engine reads the reason, not the prose: a RATE_LIMITED is worth retrying later in the same
// investigation and a NOT_PERMITTED never is.
//
// Every branch here is a **failure**, never an emptiness. There is no path in this file that turns
// an error into NO_DATA: a query that failed did not establish that the window was covered, and
// only NO_DATA is evidence that nothing happened (FR-027, FR-104).

// queryFailed is the typed refusal for a call that did not return.
func (b *Backend) queryFailed(err error, query, vocabulary string) (answer, error) {
	// A cancelled context is the caller's decision, not a fact about GCP. It propagates rather
	// than being recorded as a vendor failure the investigation would then reason about.
	if errors.Is(err, context.Canceled) {
		return answer{}, err
	}
	reason, detail := classifyFailure(err)
	return answer{
		outcome:    engine.QueryFailed{Reason: reason, Detail: detail},
		query:      query,
		vocabulary: vocabulary,
	}, nil
}

// classifyFailure maps a transport error onto the published set.
func classifyFailure(err error) (investigationv1.FailureReason, string) {
	switch {
	case errors.Is(err, errAbsentMetrics), errors.Is(err, errAbsentLogs):
		// Unreachable from the term implementations, which answer NO_DATA naming the absent
		// source before they call. It is here so that a future caller that skips that check
		// gets a typed refusal rather than an unclassified one.
		return investigationv1.FailureReason_REJECTED_BY_BACKEND, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return investigationv1.FailureReason_TIMED_OUT, "gcp: the call did not return before the deadline: " + err.Error()
	}
	// The budget's own refusal is a rate limit this process imposed on itself. It is reported as
	// one rather than as a backend rejection, because what a caller should do about it — come
	// back when the bucket has refilled — is the same thing.
	if yielded, ok := gcpx.Yielded(err); ok {
		return investigationv1.FailureReason_RATE_LIMITED,
			"gcp: the call budget yielded before this call was issued: " + yielded.Error()
	}
	if retryAfter, ok := gcpx.RateLimited(err); ok {
		return investigationv1.FailureReason_RATE_LIMITED,
			"gcp: the vendor rate-limited this call; retry after " + retryAfter.String() + ": " + err.Error()
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated:
		return investigationv1.FailureReason_NOT_PERMITTED,
			"gcp: the credential may not read this: " + err.Error()
	case codes.ResourceExhausted:
		return investigationv1.FailureReason_RATE_LIMITED, "gcp: " + err.Error()
	case codes.DeadlineExceeded, codes.Unavailable:
		return investigationv1.FailureReason_TIMED_OUT, "gcp: " + err.Error()
	case codes.InvalidArgument:
		// The selector was minted by a feeder and executed as minted (FR-111). An invalid
		// argument therefore says the stored selector no longer parses — a pointer problem, not
		// a backend one — and naming it that way is what sends the reader to the pointer.
		return investigationv1.FailureReason_UNSUPPORTED_POINTER,
			"gcp: the vendor rejected the selector as minted: " + err.Error()
	}
	if strings.Contains(strings.ToLower(err.Error()), "quota") {
		return investigationv1.FailureReason_RATE_LIMITED, "gcp: " + err.Error()
	}
	return investigationv1.FailureReason_REJECTED_BY_BACKEND, "gcp: " + err.Error()
}
