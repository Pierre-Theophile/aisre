// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Layer 2 of the gate: what the TOKEN carries, which layer 1 is blind to (research §8, item 6).
//
// `testIamPermissions` answers about the principal's IAM POLICY. A call succeeds only if the policy
// grants the permission AND the token's scopes cover it, so a credential minted with
// `cloud-platform.read-only` cannot write even where the policy would allow it — and layer 1 cannot
// tell that credential from one minted with `cloud-platform`. Those two are not equally safe, and
// the difference is invisible to the layer that people assume covers it.
//
// It also covers a case layer 1 cannot reach at all: `cloudsql.flags.list` is not an IAM permission.
// The flags endpoint is project-less and gated purely by OAuth scope, so for that call the scope is
// the only control there is (research §8.1, item 2).
//
// This is a LOCAL check against Google's tokeninfo endpoint. It spends no project quota and needs no
// permission, which is what lets it run at every startup.

// tokenInfoEndpoint is Google's public token introspection endpoint.
const tokenInfoEndpoint = "https://oauth2.googleapis.com/tokeninfo"

// ScopeCheck is layer 2's finding.
type ScopeCheck struct {
	// Scopes the token actually carries, as the vendor reports them.
	Scopes []string
	// WiderThanAsked is true when the token carries a scope broader than the read-only set this
	// integration requests. Not fatal on its own — an operator may legitimately run with an
	// environment-supplied credential — but it is the single most useful thing this layer can say,
	// because layer 1 will report a clean bill either way.
	WiderThanAsked bool
	// WriteCapableScopes names the broad scopes found, so the refusal or the warning is specific.
	WriteCapableScopes []string
	// Checked is false when the endpoint could not be reached. A failure here is NOT a pass: it is
	// reported as unverified and handed to layer 3, because "we could not look" and "we looked and
	// it was fine" are different facts.
	Checked bool
	// Err is why the check could not run, when Checked is false.
	Err string
}

// writeCapableScopes are the scopes that authorise writes across GCP. Any of these on the token
// means the token is not read-only, whatever the IAM policy says.
var writeCapableScopes = []string{
	ScopeCloudPlatform,
	"https://www.googleapis.com/auth/compute",
	"https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/logging.admin",
	"https://www.googleapis.com/auth/monitoring",
	"https://www.googleapis.com/auth/monitoring.write",
	"https://www.googleapis.com/auth/sqlservice.admin",
	"https://www.googleapis.com/auth/pubsub",
	"https://www.googleapis.com/auth/devstorage.read_write",
	"https://www.googleapis.com/auth/devstorage.full_control",
}

// CheckTokenScopes introspects the access token and reports what it may do.
//
// The token is sent to Google's own tokeninfo endpoint, which is where it came from; nothing else
// sees it, and it is never logged — the finding carries scope NAMES, never the token.
func CheckTokenScopes(ctx context.Context, accessToken string, client *http.Client) ScopeCheck {
	if accessToken == "" {
		return ScopeCheck{Checked: false, Err: "no access token to introspect"}
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		tokenInfoEndpoint+"?"+url.Values{"access_token": {accessToken}}.Encode(), nil)
	if err != nil {
		return ScopeCheck{Checked: false, Err: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ScopeCheck{Checked: false, Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ScopeCheck{Checked: false, Err: fmt.Sprintf("tokeninfo returned %d", resp.StatusCode)}
	}

	var body struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ScopeCheck{Checked: false, Err: err.Error()}
	}

	scopes := strings.Fields(body.Scope)
	check := ScopeCheck{Scopes: scopes, Checked: true}
	for _, s := range scopes {
		if slices.Contains(writeCapableScopes, s) {
			check.WiderThanAsked = true
			check.WriteCapableScopes = append(check.WriteCapableScopes, s)
		}
	}
	return check
}
