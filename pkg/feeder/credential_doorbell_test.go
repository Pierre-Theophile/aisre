// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The doorbell-only credential (006 G2, T005).

const doorbellSecret = "xapp-1-A0DOOR-1234567890-cafef00d"

func TestADoorbellCredentialRendersAsRedactedEverywhere(t *testing.T) {
	cred, err := feeder.NewDoorbellCredential("slack", doorbellSecret, []string{"connections:write"},
		[]string{"connections:write"})
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	slog.New(slog.NewTextHandler(&log, nil)).Info("doorbell", "credential", cred)
	raw, err := json.Marshal(struct{ C *feeder.DoorbellCredential }{cred})
	if err != nil {
		t.Fatal(err)
	}
	renderings := []string{
		fmt.Sprint(cred), fmt.Sprintf("%v %+v %#v %s %q %x", cred, cred, cred, cred, cred, cred),
		string(raw), log.String(),
	}
	for _, out := range renderings {
		if strings.Contains(out, "xapp") || strings.Contains(out, "cafef00d") {
			t.Errorf("the doorbell secret leaked: %s", out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("a rendering does not say it is redacted: %s", out)
		}
	}
}

func TestADoorbellCredentialIsUsedOnlyThroughOpen(t *testing.T) {
	cred, err := feeder.NewDoorbellCredential("slack", doorbellSecret, nil, []string{"connections:write"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := cred.Open(func(secret string) error { got = secret; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != doorbellSecret {
		t.Errorf("Open handed %q", got)
	}
	var none *feeder.DoorbellCredential
	if err := none.Open(func(string) error { t.Error("a nil credential opened"); return nil }); err == nil {
		t.Error("a nil credential opened without an error")
	}
}

// A doorbell credential the platform says can do more than open its connection is refused, naming
// what it holds beyond that.
func TestADoorbellCredentialThatCanDoMoreIsRefused(t *testing.T) {
	_, err := feeder.NewDoorbellCredential("slack", doorbellSecret,
		[]string{"connections:write", "authorizations:read", "app_configurations:write"},
		[]string{"connections:write"})
	var scopeErr *feeder.DoorbellScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("got %v, want a DoorbellScopeError", err)
	}
	if strings.Join(scopeErr.Extra, ",") != "app_configurations:write,authorizations:read" {
		t.Errorf("extra scopes %v", scopeErr.Extra)
	}
	for _, bad := range []struct{ platform, secret string }{{"", doorbellSecret}, {"slack", "  "}} {
		if _, err := feeder.NewDoorbellCredential(bad.platform, bad.secret, nil, nil); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}
