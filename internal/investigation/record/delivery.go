// SPDX-License-Identifier: Apache-2.0

package record

import (
	"errors"
	"time"
)

// Delivery outcomes, exactly as `investigation.report_deliveries.outcome` accepts them.
const (
	// DeliveryDelivered is a report that reached its place.
	DeliveryDelivered = "delivered"
	// DeliveryFailed is one that did not. It is recorded; the investigation still concludes.
	DeliveryFailed = "failed"
	// DeliveryNotConfigured is the ordinary case with no connector: there is nowhere to post,
	// which is not a failure of anything.
	DeliveryNotConfigured = "not_configured"
)

// ErrDeliveryFailureNeedsDetail is a failed delivery with no detail. A failure nobody can read is
// not a record of anything.
var ErrDeliveryFailureNeedsDetail = errors.New("a failed delivery must record why (FR-057f)")

// DeliveryRecord is one attempt to return a report to the place the incident lives.
type DeliveryRecord struct {
	// DeliveryID is the row's identifier; DeliveryIDFor derives the canonical one.
	DeliveryID string
	// InvestigationID is the investigation whose rendering was delivered.
	InvestigationID string
	// TargetSystem and TargetRef are the stable identifier of the place.
	TargetSystem string
	TargetRef    string
	// ExternalMessageRef is what is edited in place on the next update. Empty until the first
	// successful delivery mints one.
	ExternalMessageRef string
	// RenderingDigest is sha256 of the rendered report that was delivered.
	RenderingDigest string
	// Outcome is one of the three above.
	Outcome string
	// FailureDetail is required when Outcome is DeliveryFailed.
	FailureDetail string
	// At is when the attempt happened. Zero means now.
	At time.Time
}

// DeliveryIDFor is the canonical delivery id: one row per (investigation, target system, target
// ref), which is what makes "edited in place" representable.
func DeliveryIDFor(investigationID, targetSystem, targetRef string) string {
	return "delivery-" + investigationID + "-" + targetSystem + "-" + targetRef
}
