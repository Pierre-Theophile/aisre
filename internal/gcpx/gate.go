// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

// The gate: the three layers in order, and the refusal names which one objected (FR-004).
//
// This is the only constructor of a *Credential in this package, which is what makes the type an
// assertion rather than a wrapper. A caller cannot hold a Credential that did not pass, and a
// per-area client cannot be built without one (internal/feeders/gcp/transport.go), so there is no
// ordering of this program in which GCP is read before the check that makes reading it safe.
//
// Layer order is deliberate: layer 1 is the cheapest and catches the common catastrophe (someone
// bound roles/editor to the connector's service account), layer 2 catches what layer 1 is
// structurally blind to, and layer 3 is what a person signs for. Running them in the other order
// would make an operator assert around a failure the machine could have found.

// GateOptions configures a gate run.
type GateOptions struct {
	// Projects to test. Empty is refused: a gate over no project proves nothing, and FR-131 makes
	// scope configuration that an operator sets rather than a default this code invents.
	Projects []string
	// Assertion is layer 3's operator declaration.
	Assertion *Assertion
	// ChunkSize seeds layer 1's chunking. Zero uses DefaultPermissionChunk.
	ChunkSize int
	// HTTPClient is used for the tokeninfo call; nil gets a default with a timeout.
	HTTPClient *http.Client
	// Usage receives the gate's own calls, counted apart from ingestion and investigation so
	// their cost is visible rather than folded into either (FR-113).
	Usage *Usage
	// SkipTokenScope turns off layer 2. It exists for the recorded-replay path, where there is no
	// live token to introspect — NOT as an operator switch, and the result records that the layer
	// did not run so a reader is never told a check happened that did not.
	SkipTokenScope bool
}

// Gate runs the three layers and returns a Credential on success.
func Gate(ctx context.Context, opts GateOptions) (*Credential, error) {
	if len(opts.Projects) == 0 {
		return nil, &GateRefusal{
			Layer: LayerIAM,
			Detail: "no project in scope. A read-only check over zero projects proves nothing, and " +
				"the scope is operator configuration (FR-131) rather than a default this code invents",
		}
	}

	creds, err := FindDefaultCredential(ctx)
	if err != nil {
		return nil, err
	}

	result := &GateResult{
		Unverifiable: UnverifiableAreas(),
		ChunkSize:    opts.ChunkSize,
	}

	// ---- layer 1 ----
	tester, err := NewIAMTester(ctx, option.WithTokenSource(creds.TokenSource), opts.Usage)
	if err != nil {
		return nil, err
	}
	if err := runIAMLayer(ctx, tester, opts, result); err != nil {
		return nil, err
	}

	// ---- layer 2 ----
	if err := runScopeLayer(ctx, creds, opts, result); err != nil {
		return nil, err
	}

	// ---- layer 3 ----
	if err := opts.Assertion.Validate(result.Unverifiable); err != nil {
		var refusal *GateRefusal
		if errors.As(err, &refusal) {
			refusal.Result = result
		}
		return nil, err
	}
	result.AssertedBy = opts.Assertion.By

	return &Credential{
		creds:     creds,
		projectID: creds.ProjectID,
		scopes:    ReadOnlyScopes,
		gate:      result,
	}, nil
}

// runIAMLayer tests the enumerated write set on every in-scope project.
func runIAMLayer(ctx context.Context, tester IAMTester, opts GateOptions, result *GateResult) error {
	perms := WritePermissions()
	result.Tested = len(perms)

	heldSet := map[string]bool{}
	chunk := opts.ChunkSize
	for _, project := range opts.Projects {
		held, settled, err := TestWritePermissions(ctx, tester, project, perms, chunk)
		if err != nil {
			return &GateRefusal{
				Layer: LayerIAM,
				Detail: fmt.Sprintf("could not ask GCP what this principal may do on %s: %v. "+
					"A gate that cannot run is not a gate that passed", project, err),
				Result: result,
			}
		}
		chunk = settled
		for _, p := range held {
			heldSet[project+": "+p] = true
		}
	}
	result.ChunkSize = chunk

	for p := range heldSet {
		result.Held = append(result.Held, p)
	}
	sort.Strings(result.Held)

	if len(result.Held) > 0 {
		return &GateRefusal{
			Layer: LayerIAM,
			Detail: fmt.Sprintf("this principal holds %d write permission(s) in areas this "+
				"integration reads. Read-only is a property of the credential, not of the code's "+
				"restraint, so the integration refuses to start:\n  - %s",
				len(result.Held), strings.Join(result.Held, "\n  - ")),
			Result: result,
		}
	}
	return nil
}

// runScopeLayer introspects the token.
//
// A token WIDER than asked is reported, not refused. That is a judgement worth stating: an
// environment-supplied credential carrying cloud-platform is extremely common (it is what the
// metadata server hands out by default), and refusing it outright would push operators to disable
// the gate entirely — trading a loud, accurate warning for silence. It is recorded in the result,
// surfaced in the usage report, and it is precisely what layer 3 asks a person to sign for.
func runScopeLayer(ctx context.Context, creds *google.Credentials, opts GateOptions, result *GateResult) error {
	if opts.SkipTokenScope {
		result.Unverifiable = append(result.Unverifiable,
			"OAuth token scope: layer 2 did not run in this mode, so nothing here checked what the "+
				"token itself may do")
		return nil
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		result.Unverifiable = append(result.Unverifiable,
			"OAuth token scope: the token could not be obtained to introspect ("+err.Error()+")")
		return nil
	}
	check := CheckTokenScopes(ctx, token.AccessToken, opts.HTTPClient)
	result.Scopes = check.Scopes
	result.ScopeWiderThanAsked = check.WiderThanAsked
	if !check.Checked {
		result.Unverifiable = append(result.Unverifiable,
			"OAuth token scope: the check could not run ("+check.Err+"), which is not the same as "+
				"passing")
		return nil
	}
	if check.WiderThanAsked {
		result.Unverifiable = append(result.Unverifiable, fmt.Sprintf(
			"OAuth token scope: the token carries %s, which is broader than the read-only set this "+
				"integration requests. Layer 1 cannot see this at all, so it is layer 3's to accept",
			strings.Join(check.WriteCapableScopes, ", ")))
	}
	return nil
}
