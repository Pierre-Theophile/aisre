// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The startup gate (T058; contracts/read-only-operations.md §3, FR-003).
//
// Nothing is emitted before it passes. It proves two things, and says which one it could not:
//
//  1. the keys work — `validate_keys` answers, else the start is refused naming the key;
//  2. the application key can read and nothing else — its own scopes are asked for, and any scope
//     outside the read set of the enabled capabilities refuses the start, naming each one.
//
// Where Datadog will not say (the key lacks the permission to read its own scopes, no key id was
// given, or the key is unscoped and therefore carries every permission of its user), the gate names
// what it could not verify and requires a named operator's assertion, which every checkpoint then
// records as `operator_asserted`. Without it the connector does not start.

// ReadScopes are the Datadog permissions each capability needs (§4). Names not yet confirmed against
// Datadog's permissions reference are marked in the contract, and corrected there if wrong.
var ReadScopes = map[Capability][]string{
	CapLogs:        {"logs_read_data", "logs_read_index_data"},
	CapMonitors:    {"monitors_read"},
	CapTags:        nil,
	CapAPMTopology: {"apm_read", "apm_service_catalog_read"},
	CapChanges:     {"events_read"},
}

// AllowedScopes is the read set of the enabled capabilities, sorted.
func AllowedScopes(caps Capabilities) []string {
	var out []string
	for _, c := range AllCapabilities {
		if caps.Enabled(c) {
			out = append(out, ReadScopes[c]...)
		}
	}
	sort.Strings(out)
	return out
}

// GateReader is the two reads the gate makes. datadogx.Client satisfies it through gateClient.
type GateReader interface {
	// ValidateKeys calls `validate_keys`.
	ValidateKeys(ctx context.Context) error
	// OwnAppKeyScopes reads the application key's scopes. scoped is false for an unscoped key.
	// A 403 is reported as ErrScopesUnreadable.
	OwnAppKeyScopes(ctx context.Context, keyID string) (scopes []string, scoped bool, err error)
}

// ErrScopesUnreadable is Datadog refusing to tell a key its own scopes.
var ErrScopesUnreadable = errors.New("datadog: the application key may not read its own scopes")

// GateOptions configures the gate.
type GateOptions struct {
	Capabilities Capabilities
	// AppKeyID identifies the application key whose scopes are read. Empty means they cannot be.
	AppKeyID string
	// AssertedBy is the named individual asserting the key is read-only, for what could not be
	// verified. Empty means no assertion.
	AssertedBy string
	Now        func() time.Time
}

// GateVerdict is what the gate found.
type GateVerdict struct {
	// Scopes are the key's scopes when Datadog stated them.
	Scopes []string
	// Unverified names what could not be verified; non-empty means the start rests on Assertion.
	Unverified []string
	// Assertion is "operator_asserted by <name> at <instant>" when the start rests on one.
	Assertion string
}

// String renders the verdict for --dry-run and the checkpoint.
func (v GateVerdict) String() string {
	lines := []string{"key scopes: " + orUnset(strings.Join(v.Scopes, ", "))}
	for _, u := range v.Unverified {
		lines = append(lines, "not verifiable: "+u)
	}
	if v.Assertion != "" {
		lines = append(lines, v.Assertion)
	}
	return strings.Join(lines, "\n")
}

// Gate runs the startup gate.
func Gate(ctx context.Context, r GateReader, opts GateOptions) (GateVerdict, error) {
	// The gate's reads are the connector starting, not an investigation: the usage report says so.
	ctx = WithArea(ctx, AreaStartup)
	if err := r.ValidateKeys(ctx); err != nil {
		return GateVerdict{}, fmt.Errorf("datadog: validate_keys refused the keys, so nothing will start: %w", err)
	}
	var v GateVerdict
	if opts.AppKeyID == "" {
		v.Unverified = append(v.Unverified, "the application key's scopes: no application key id was given, so they "+
			"cannot be read")
	} else {
		scopes, scoped, err := r.OwnAppKeyScopes(ctx, opts.AppKeyID)
		switch {
		case errors.Is(err, ErrScopesUnreadable):
			v.Unverified = append(v.Unverified, "the application key's scopes: Datadog refused to state them (the key "+
				"lacks the permission to read its own application keys)")
		case err != nil:
			return GateVerdict{}, fmt.Errorf("datadog: reading the application key's scopes: %w", err)
		case !scoped:
			v.Unverified = append(v.Unverified, "the application key's scopes: the key is unscoped, so it carries every "+
				"permission of its user, writes included; scope it to "+strings.Join(AllowedScopes(opts.Capabilities), ", "))
		default:
			sort.Strings(scopes)
			v.Scopes = scopes
			allowed := map[string]bool{}
			for _, s := range AllowedScopes(opts.Capabilities) {
				allowed[s] = true
			}
			var offending []string
			for _, s := range scopes {
				if !allowed[s] {
					offending = append(offending, s)
				}
			}
			if len(offending) > 0 {
				return GateVerdict{}, fmt.Errorf("datadog: the application key carries scopes outside the read set of "+
					"the enabled capabilities (%s): %s. Nothing will start; scope the key down",
					strings.Join(AllowedScopes(opts.Capabilities), ", "), strings.Join(offending, ", "))
			}
		}
	}
	if len(v.Unverified) > 0 {
		if strings.TrimSpace(opts.AssertedBy) == "" {
			return v, fmt.Errorf("datadog: the gate could not verify %s. Nothing will start without "+
				"--assert-read-only <name>: a named individual asserting the key is read-only, recorded in every "+
				"checkpoint", strings.Join(v.Unverified, "; "))
		}
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		v.Assertion = fmt.Sprintf("operator_asserted by %s at %s", opts.AssertedBy, now().UTC().Format(time.RFC3339))
	}
	return v, nil
}

// ---- the reads, over the declared operations ----------------------------------------------------------

// Doer is the one method of datadogx.Client the gate uses; an interface so this package does not import
// the client (the client imports this package's surface).
type Doer interface {
	DoRaw(ctx context.Context, op, path string) (status int, body []byte, err error)
}

// GateClient adapts a Doer to GateReader.
type GateClient struct{ Doer Doer }

// ValidateKeys calls `validate_keys`.
func (g GateClient) ValidateKeys(ctx context.Context) error {
	status, _, err := g.Doer.DoRaw(ctx, string(OpValidateKeys), "/api/v2/validate_keys")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("validate_keys answered %d", status)
	}
	return nil
}

// OwnAppKeyScopes reads the key's own application-key record.
func (g GateClient) OwnAppKeyScopes(ctx context.Context, keyID string) ([]string, bool, error) {
	status, body, err := g.Doer.DoRaw(ctx, string(OpOwnAppKey), "/api/v2/current_user/application_keys/"+keyID)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, false, ErrScopesUnreadable
	default:
		return nil, false, fmt.Errorf("the application key read answered %d", status)
	}
	var out struct {
		Data struct {
			Attributes struct {
				Scopes []string `json:"scopes"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, fmt.Errorf("the application key read no longer parses: %w", err)
	}
	scopes := out.Data.Attributes.Scopes
	return scopes, scopes != nil, nil
}
