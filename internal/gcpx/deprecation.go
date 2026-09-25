// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
)

// `Deprecation` and `Sunset` on the GCP side (T082, FR-060, research §10).
//
// Every call this integration makes to a Google API is a chance to be told, by the vendor, on the
// response, that the thing being called is going away. RFC 9745 and RFC 8594 are the only
// standardised way anybody says so, and reading them costs one header lookup on a response already in
// hand — no credential, no cadence, no quota.
//
// # Why it is reported here rather than emitted as a vendor notice
//
// The obvious move is to turn a header the GCP feeder saw into a vendor-notice announcement. It is
// the wrong one: FR-002 gives the two feeders separate source ids, cadences and checkpoints, and one
// feeder writing into another's source id would make the vendor-notice extent a claim about a window
// the vendor-notice feeder never read. A gap in the mailbox would then be reported against events the
// mailbox never carried.
//
// So a GCP run **reports** what it saw, in its own run and against its own source, and the
// vendor-notice feeder reads the same headers on the hosts *it* polls. The same package parses both
// (internal/deprecation), so the two readings cannot disagree about what a header meant.

// WithDeprecationCapture wraps a client's transport so every response it carries is checked for the
// two headers. It returns a client sharing the original's settings: the caller keeps its timeout, its
// redirect policy and its cookie jar, because a capture that quietly replaced any of those would be
// changing the behaviour of every call in the process.
func WithDeprecationCapture(client *http.Client, log *deprecation.Log) *http.Client {
	if log == nil {
		if client == nil {
			return &http.Client{Timeout: 10 * time.Second}
		}
		return client
	}
	if client == nil {
		return &http.Client{
			Timeout:   10 * time.Second,
			Transport: deprecation.Transport(nil, log),
		}
	}
	wrapped := *client
	wrapped.Transport = deprecation.Transport(client.Transport, log)
	return &wrapped
}

// DeprecationReport renders what a run was told, for the run's own output.
//
// One line per resource, with what was stated and what was not. A resource that stated only a sunset
// says so rather than being given an invented deprecation instant: when a vendor did not say when
// something was deprecated, the report does not either.
//
// An empty report is printed by the caller as "nothing stated", never omitted. The absence of a
// deprecation header is a fact about the run — it means no API we called said it was going away — and
// a report that simply vanished when it was empty would leave a reader unable to tell that from a run
// that never looked.
func DeprecationReport(notices []deprecation.Notice) []string {
	sorted := append([]deprecation.Notice(nil), notices...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].URL < sorted[j].URL })
	out := make([]string, 0, len(sorted))
	for _, notice := range sorted {
		parts := []string{notice.URL}
		switch {
		case notice.Deprecated.IsZero():
			parts = append(parts, "deprecated: not stated")
		default:
			parts = append(parts, "deprecated "+notice.Deprecated.UTC().Format(time.RFC3339))
		}
		switch {
		case notice.Sunset.IsZero():
			parts = append(parts, "sunset: not stated")
		default:
			parts = append(parts, "sunset "+notice.Sunset.UTC().Format(time.RFC3339))
		}
		out = append(out, strings.Join(parts, " — "))
	}
	return out
}

// DeprecationSummary is the one-line form for a checkpoint note or a log field.
func DeprecationSummary(notices []deprecation.Notice) string {
	if len(notices) == 0 {
		return "no API this run called stated a deprecation or a sunset"
	}
	hosts := map[string]bool{}
	for _, notice := range notices {
		hosts[notice.Host] = true
	}
	names := make([]string, 0, len(hosts))
	for host := range hosts {
		names = append(names, host)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d deprecation or sunset headers from %s", len(notices), strings.Join(names, ","))
}
