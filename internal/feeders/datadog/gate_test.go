// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
)

type reader struct {
	validErr error
	scopes   []string
	scoped   bool
	scopeErr error
}

func (r reader) ValidateKeys(context.Context) error { return r.validErr }

func (r reader) OwnAppKeyScopes(context.Context, string) ([]string, bool, error) {
	return r.scopes, r.scoped, r.scopeErr
}

func TestTheStartupGate(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }
	caps := ddfeeder.DefaultCapabilities()
	for _, tc := range []struct {
		name   string
		r      reader
		opts   ddfeeder.GateOptions
		refuse string
		assert bool
	}{
		{"keys refused", reader{validErr: errors.New("403")}, ddfeeder.GateOptions{AppKeyID: "k"}, "validate_keys", false},
		{"read scopes only", reader{scopes: []string{"monitors_read", "logs_read_data"}, scoped: true}, ddfeeder.GateOptions{AppKeyID: "k"}, "", false},
		{"a write scope", reader{scopes: []string{"monitors_read", "monitors_write"}, scoped: true}, ddfeeder.GateOptions{AppKeyID: "k"}, "monitors_write", false},
		{"unscoped, no assertion", reader{scoped: false}, ddfeeder.GateOptions{AppKeyID: "k"}, "--assert-read-only", false},
		{"unscoped, asserted", reader{scoped: false}, ddfeeder.GateOptions{AppKeyID: "k", AssertedBy: "A. Operator"}, "", true},
		{"scopes unreadable, asserted", reader{scopeErr: ddfeeder.ErrScopesUnreadable}, ddfeeder.GateOptions{AppKeyID: "k", AssertedBy: "A. Operator"}, "", true},
		{"no key id, no assertion", reader{}, ddfeeder.GateOptions{}, "no application key id", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Capabilities, tc.opts.Now = caps, now
			v, err := ddfeeder.Gate(context.Background(), tc.r, tc.opts)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("err %v, want a refusal naming %q", err, tc.refuse)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.HasPrefix(v.Assertion, "operator_asserted by A. Operator"); got != tc.assert {
				t.Errorf("assertion %q", v.Assertion)
			}
		})
	}
}
