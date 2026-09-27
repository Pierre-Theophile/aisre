// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

const newCommit = "0123456789abcdef0123456789abcdef01234567"

// versionTwin answers the two aggregates: the error one (its query carries status:error) and the total.
func versionTwin(errors, totals string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "status:error") {
			_, _ = io.WriteString(w, errors)
			return
		}
		_, _ = io.WriteString(w, totals)
	}
}

func buckets(rows ...string) string {
	return `{"data":{"buckets":[` + strings.Join(rows, ",") + `]},"meta":{"status":"done"}}`
}

func bucket(version string, n int) string {
	return `{"by":{"version":"` + version + `"},"computes":{"c0":` + strconv.Itoa(n) + `}}`
}

// errors_by_version groups by the stamp, names each group's deploy reference, keeps the lines with
// no stamp as their own group, and puts the worst error rate first (FR-040b–FR-040d).
func TestErrorsByVersionNamesEachGroupsDeployReference(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, versionTwin(
		buckets(bucket(newCommit, 40), bucket("v1.2.3", 2), bucket("__no_version_stamp__", 1)),
		buckets(bucket(newCommit, 100), bucket("v1.2.3", 200), bucket("__no_version_stamp__", 10)),
	))
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(logPointer(),
		engine.NewWindow(at.Add(-time.Hour), at), "version")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	if *calls != 2 {
		t.Errorf("%d Datadog calls, want 2 aggregates", *calls)
	}
	rows := resp.GetDigest().GetErrorsByVersion().GetVersions()
	if len(rows) != 3 {
		t.Fatalf("%d groups, want 3: %v", len(rows), rows)
	}

	first := rows[0]
	if first.GetVersion() != newCommit || first.GetErrors() != 40 || first.GetTotal() != 100 || first.GetErrorRate() != 0.4 {
		t.Errorf("worst group %v", first)
	}
	if ref := first.GetDeployRef(); ref.GetNamespace() != "deploy.commit_sha" || ref.GetValue() != newCommit {
		t.Errorf("commit group deploy_ref %v", ref)
	}
	if first.GetDrillDown() == nil {
		t.Errorf("the commit group has no drill-down handle")
	}

	missing, release := rows[1], rows[2]
	if missing.GetVersion() != "" || missing.GetDeployRef() != nil ||
		missing.GetDeployRefAbsentReason() != investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER {
		t.Errorf("unstamped group %v", missing)
	}
	if release.GetVersion() != "v1.2.3" || release.GetDeployRef().GetNamespace() != "deploy.release" {
		t.Errorf("release group %v", release)
	}

	cov := resp.GetDigest().GetCoverage()
	if cov.GetVolumeConsidered() != 310 {
		t.Errorf("coverage volume %d, want 310", cov.GetVolumeConsidered())
	}
}

// No line in the window is NO_DATA, not an empty digest.
func TestErrorsByVersionWithNoLinesIsNoData(t *testing.T) {
	t.Parallel()
	b, _ := liveBackend(t, versionTwin(buckets(), buckets()))
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(logPointer(),
		engine.NewWindow(at.Add(-time.Hour), at), "version")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome %v", resp.GetOutcome())
	}
}

// Datadog's refusals map to the typed failure reasons, never to NO_DATA.
func TestErrorsByVersionMapsDatadogRefusals(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]investigationv1.FailureReason{
		http.StatusTooManyRequests: investigationv1.FailureReason_RATE_LIMITED,
		http.StatusForbidden:       investigationv1.FailureReason_NOT_PERMITTED,
		http.StatusGatewayTimeout:  investigationv1.FailureReason_TIMED_OUT,
		http.StatusBadRequest:      investigationv1.FailureReason_REJECTED_BY_BACKEND,
	} {
		b, _ := liveBackend(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"errors":["no"]}`)
		})
		resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(logPointer(),
			engine.NewWindow(at.Add(-time.Hour), at), "version")))
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED || resp.GetFailureReason() != want {
			t.Errorf("%d: got %v / %v, want %v", status, resp.GetOutcome(), resp.GetFailureReason(), want)
		}
	}
}
