// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Every call goes through the metered door (004 T081, FR-004, FR-031).
//
// The read-only surface is only a promise until something proves the transport consults it. These
// tests are that proof: a refused operation must not reach the network, and the calls that are on the
// surface must be the ones the published table names.

func issuer(t *testing.T) *feeder.Issuer {
	t.Helper()
	i, err := feeder.NewIssuer(vercelfeeder.Surface, nil, nil)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return i
}

// A transport with no issuer is refused at construction rather than silently unmetered.
func TestATransportNeedsAnIssuer(t *testing.T) {
	t.Parallel()
	if _, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{}); err == nil {
		t.Error("a client was built with no issuer; every call is supposed to go through the metered door")
	}
}

// The listing reaches the published path, carries the platform's own filters, and sends the token.
//
// The target filter is asserted because FR-032's exclusion has to be the PLATFORM's statement: a
// preview the server never sent is one this connector never paid for, and a client-side filter would
// spend quota to discard.
func TestDeploymentsCallsThePublishedOperation(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"deployments":[{"uid":"dpl_42","target":"production"}]}`))
	}))
	defer srv.Close()

	c, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{
		BaseURL: srv.URL, Issuer: issuer(t), TeamID: "team_1",
		Token: func(context.Context) (string, error) { return "tok", nil },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got, err := c.Deployments(context.Background(), "prj_1", "production", vercelfeeder.ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(got) != 1 || got[0].UID != "dpl_42" {
		t.Fatalf("decoded %+v", got)
	}
	if gotPath != "/v7/deployments" {
		t.Errorf("path = %q, want the published /v7/deployments", gotPath)
	}
	for _, want := range []string{"projectId=prj_1", "target=production", "limit=50", "teamId=team_1"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("authorization = %q", gotAuth)
	}
}

// Environment metadata never asks to decrypt, and the decoded type has nowhere to put a value.
func TestProjectEnvNeverAsksForAValue(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		// The platform sends a value anyway. It must land nowhere.
		_, _ = w.Write([]byte(`{"envs":[{"id":"e1","key":"DATABASE_URL","value":"postgres://u:p@h/db","type":"encrypted"}]}`))
	}))
	defer srv.Close()

	c, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{BaseURL: srv.URL, Issuer: issuer(t)})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	envs, err := c.ProjectEnv(context.Background(), "prj_1")
	if err != nil {
		t.Fatalf("ProjectEnv: %v", err)
	}
	if strings.Contains(gotQuery, "decrypt") {
		t.Errorf("the request asked to decrypt: %q", gotQuery)
	}
	if len(envs) != 1 || envs[0].Key != "DATABASE_URL" {
		t.Fatalf("decoded %+v", envs)
	}
	// The decisive assertion: the struct has no field that could hold the secret the server sent.
	// A future field would fail this by construction rather than by review.
	for _, field := range structFields(envs[0]) {
		if strings.EqualFold(field, "value") || strings.EqualFold(field, "decrypted") {
			t.Errorf("ProjectEnv has a %q field; the decrypted value would have somewhere to land", field)
		}
	}
}

// An operation the surface does not carry is refused before anything leaves the process.
func TestAnUnpublishedOperationNeverReachesTheNetwork(t *testing.T) {
	t.Parallel()
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()

	narrow, err := feeder.NewReadOnlySurface(vercelfeeder.Platform,
		map[feeder.ReadOperation]feeder.ReadOperationSpec{
			vercelfeeder.OpProjects: {Area: "projects", Why: "only this one is published here"},
		})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	i, err := feeder.NewIssuer(narrow, nil, nil)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	c, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{BaseURL: srv.URL, Issuer: i})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := c.Deployments(context.Background(), "prj_1", "production", vercelfeeder.ListWindow{}); err == nil {
		t.Error("an unpublished operation was issued")
	}
	if reached {
		t.Error("the request reached the server; the surface gate is supposed to refuse before the wire")
	}
}

// structFields lists a struct's field names, so a test can assert a field's ABSENCE.
func structFields(v any) []string {
	t := reflect.TypeOf(v)
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		out = append(out, t.Field(i).Name)
	}
	return out
}
