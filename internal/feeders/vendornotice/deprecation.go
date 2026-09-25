// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
)

// `Deprecation` and `Sunset` (T082, FR-060, research §10).
//
// These two headers are **the only standardised deprecation source there is**. Everything else this
// feeder reads is a human writing prose in a medium of their choosing; RFC 9745 (`Deprecation`,
// Standards Track) and RFC 8594 (`Sunset`, Informational) are a machine-readable statement, from the
// vendor, on the response to a call the integration was already making.
//
// So they are near-free: no new credential, no new cadence, no new budget — an observation on traffic
// that already flows. The cost of missing them is the same as the cost of missing a maintenance email,
// and the coverage audit is what says that cost is real.
//
// # What each one means, and why the pair maps onto a window
//
// `Deprecation` states when the resource became — or will become — deprecated. `Sunset` states when it
// will stop responding. Together they are an interval, and an interval is exactly what an announced
// fact is: a DEPRECATION change whose valid time may begin in the past (the vendor deprecated it last
// year) and end in the future (it goes away in March).
//
// Where only a sunset is stated, the valid start is marked **unknown** rather than guessed at the
// instant we happened to notice (FR-069). The vendor did not say when it was deprecated; the honest
// record says so. Where only a deprecation is stated, the window is open-ended, which is what an
// undated removal is.
//
// # Two spellings of `Deprecation`, and only one of `Sunset`
//
// RFC 9745 publishes `Deprecation` as a structured-field Date — `@1735689600`, seconds since the
// epoch. The draft that circulated for years before it used an HTTP-date, and deployed vendors still
// send that. Both are read, because refusing the older spelling would mean reading nothing from the
// APIs most likely to be deprecating something.
//
// `Sunset` is an HTTP-date and nothing else (RFC 8594 §3). A `@`-form `Sunset` is not a Sunset header,
// and reading one as a date would be inventing a removal instant from a field that does not carry one.

// DeprecationHeaders turns the headers seen this cycle into announcements the rest of the pipeline
// treats like any other.
//
// The attribution is the allowlist's, by host — the same configured assertion FR-119's rule rests on.
// A header from a host nobody mapped to a vendor is not this organisation's dependency to record, and
// it is returned as a drop rather than as an announcement about a vendor invented from a domain name.
//
// The product cannot be read from a header, so it is read from configuration: the host's stated
// product where the allowlist maps one, the vendor's only product where it has exactly one, and
// otherwise **unextracted** naming the product field. That last case is the honest one and it is meant
// to be visible: a `Sunset` on one of eleven allowlisted products is a real notice that nobody can
// attribute, and a guess would put the removal date on the wrong thing.
type DeprecationHeaders struct {
	log   *deprecation.Log
	list  *Allowlist
	kind  SourceKind
	drops []Drop
}

// NewDeprecationHeaders builds the adapter over a log the shared client fills.
func NewDeprecationHeaders(log *deprecation.Log, list *Allowlist) *DeprecationHeaders {
	return &DeprecationHeaders{log: log, list: list, kind: SourceDeprecationHeader}
}

// Kind implements Announcer.
func (d *DeprecationHeaders) Kind() SourceKind { return d.kind }

// Read drains the log. It never fails: the headers were observed on requests made for other reasons,
// so there is no separate poll to be unreachable — which is exactly why this source can never produce
// a gap and the other three can.
func (d *DeprecationHeaders) Read(_ context.Context) (Batch, error) {
	var batch Batch
	d.drops = nil
	for _, notice := range d.log.Take() {
		vendor, known := d.list.VendorByHost(notice.Host)
		if !known {
			d.drops = append(d.drops, Drop{
				Reason: DropVendorNotAllowlisted, Vendor: notice.Host, Pointer: notice.URL,
			})
			continue
		}
		product, ok := productFor(vendor, notice.Host)
		if !ok {
			batch.Unextracted = append(batch.Unextracted, Unextracted{
				Reason: UnextractedNoProduct, Field: "product",
				Pointer: notice.URL, Source: d.kind,
			})
			continue
		}
		batch.Announcements = append(batch.Announcements, Announcement{
			Vendor: vendor.Slug, Product: product, Kind: KindDeprecation,
			Window:  windowFromHeaders(notice),
			Pointer: notice.URL, Source: d.kind,
		})
	}
	return batch, nil
}

// Drops returns what the last Read attributed to no allowlisted vendor, for the cycle report.
func (d *DeprecationHeaders) Drops() []Drop { return d.drops }

// windowFromHeaders maps the pair onto an announced interval.
//
// `Deprecation` states when the resource became — or will become — deprecated; `Sunset` states when it
// stops responding. Together they are an interval, which is exactly what an announced fact is: a
// DEPRECATION change whose valid time may begin in the past (deprecated last year) and end in the
// future (removed in March).
//
// Where only a sunset is stated, the valid start is marked **unknown** rather than guessed at the
// instant we happened to notice (FR-069). The vendor did not say when it was deprecated, and the
// honest record says so. Where only a deprecation is stated, the window is open-ended, which is what
// an undated removal is.
func windowFromHeaders(notice deprecation.Notice) Window {
	if notice.Deprecated.IsZero() {
		return Window{StartUnknown: true, End: notice.Sunset}
	}
	return Window{Start: notice.Deprecated, End: notice.Sunset}
}

// productFor decides which allowlisted product a host's deprecation is about.
func productFor(vendor Vendor, host string) (string, bool) {
	if product := vendor.productForHost(host); product != "" {
		return product, true
	}
	if len(vendor.Products) == 1 {
		return strings.ToLower(strings.TrimSpace(vendor.Products[0])), true
	}
	return "", false
}
