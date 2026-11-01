// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"errors"
	"slices"
)

// The role names the API grants (internal/server.Role). They are repeated here as strings because
// this package is a client of the API and does not link the server.
const (
	roleReader       = "reader"
	roleDecider      = "decider"
	roleFeeder       = "feeder"
	roleInvestigator = "investigator"
)

// ErrNoUsableRole is returned when the caller holds no role any tool needs, which is the case of a
// feeder token (010 FR-014) and of a token without roles.
var ErrNoUsableRole = errors.New("mcp: the caller holds neither the reader nor the investigator role; " +
	"a feeder token cannot be used here")

// Roles is what WhoAmI said the caller was granted.
type Roles []string

// has applies the API's implications (internal/server.Principal.Has): decider and investigator
// both imply reader; nothing implies investigator.
func (r Roles) has(role string) bool {
	if slices.Contains(r, role) {
		return true
	}
	if role == roleReader {
		return slices.Contains(r, roleDecider) || slices.Contains(r, roleInvestigator)
	}
	return false
}

// usable reports whether at least one tool would be listed for these roles.
func (r Roles) usable() bool { return r.has(roleReader) || r.has(roleInvestigator) }
