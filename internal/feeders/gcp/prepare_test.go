// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// A recorded poll marker keeps a reason, because a partial checkpoint without one is refused, and the
// reason it keeps names operations and nothing else.
func TestARecordedPollReasonNamesOperationsOnly(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(gcpfeeder.PollMarker{
		Outcome: "partial",
		Reason: "run.revisions.list acme-prod/europe-west1: googleapi: Error 503 for acme-prod; " +
			"cloudsql.instances.list acme-prod: denied; run.revisions.list acme-prod/us-east1: timeout",
		Projects: []string{"acme-prod"},
	})
	key, _ := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	san, _ := sanitise.New(sanitise.ContractPolicy(), key)
	prepare := gcpfeeder.RecordingPrepare(san)
	out, err := prepare(gcpfeeder.PayloadPollMarker, raw)
	if err != nil {
		t.Fatal(err)
	}
	var marker gcpfeeder.PollMarker
	if err := json.Unmarshal(out, &marker); err != nil {
		t.Fatal(err)
	}
	if want := "failed reads: cloudsql.instances.list, run.revisions.list"; marker.Reason != want {
		t.Errorf("reason = %q, want %q", marker.Reason, want)
	}
	if strings.Contains(marker.Reason, "acme") {
		t.Error("the recorded reason still names the project")
	}
	if len(marker.Projects) != 1 {
		t.Error("the pre-pass touched a field it is not about; the table pseudonymises projects")
	}
	other := []byte(`{"services":[]}`)
	if got, _ := prepare(gcpfeeder.PayloadSQLFlags, other); string(got) != string(other) {
		t.Error("the pre-pass changed a payload that is not a poll marker")
	}
}

// An environment variable's value reaches a recording only as the one Cloud SQL connection name the
// env_var_value derivation reads from it, pseudonymised as the volume field is (FR-034 as amended,
// FR-028). Everything else in the value — a DSN's user, password, database, host — never survives,
// in any form.
func TestARecordedEnvValueKeepsOnlyItsConnectionName(t *testing.T) {
	t.Parallel()
	key, err := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	connection := func(project, instance string) string {
		p, _ := san.LabelIdentifier(sanitise.KindProject, project)
		i, _ := san.LabelIdentifier(sanitise.KindInstance, instance)
		return p + ":europe-west1:" + i
	}
	cases := []struct {
		name, value string
		want        string // "" means the value is removed
		mustNotHave []string
	}{
		{"the connector's own variable", "acme-prod:europe-west1:orders-db", connection("acme-prod", "orders-db"), []string{"acme-prod", "orders-db"}},
		{"a DSN with a password beside the socket", "postgres://svc:S3cr3tPw@/orders?host=/cloudsql/acme-prod:europe-west1:orders-db",
			connection("acme-prod", "orders-db"), []string{"S3cr3tPw", "svc:", "orders?", "postgres", "acme-prod"}},
		{"a password that is itself colon-shaped", "postgres://u:ab:us-east1:cd@/db?host=/cloudsql/acme-prod:europe-west1:orders-db",
			connection("acme-prod", "orders-db"), []string{"ab:us-east1", ":cd", "u:"}},
		{"two connection names keep the feeder's first", "acme-prod:europe-west1:orders-db,acme-prod:europe-west1:billing-db",
			connection("acme-prod", "orders-db"), []string{"billing"}},
		{"a value with no connection name", "redis://cache.internal:6379/0", "", []string{"redis", "cache.internal", "6379"}},
		{"three parts that are not one", "a:b:c", "", []string{"a:b:c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, _ := json.Marshal(map[string]any{"revisions": []any{map[string]any{
				"containers": []any{map[string]any{"env": []any{map[string]any{"name": "DATABASE_URL", "value": tc.value}}}},
			}}})
			prepared, err := gcpfeeder.RecordingPrepare(san)(gcpfeeder.PayloadRevisions, raw)
			if err != nil {
				t.Fatal(err)
			}
			out, err := san.JSON("gcp."+gcpfeeder.PayloadRevisions, prepared)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Revisions []struct {
					Containers []struct {
						Env []map[string]string `json:"env"`
					} `json:"containers"`
				} `json:"revisions"`
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatal(err)
			}
			entry := decoded.Revisions[0].Containers[0].Env[0]
			if entry["name"] != "DATABASE_URL" {
				t.Errorf("the variable's name was lost: %v", entry)
			}
			if got := entry["value"]; got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
			for _, leak := range tc.mustNotHave {
				if strings.Contains(string(out), leak) {
					t.Errorf("%q survived in the recording: %s", leak, out)
				}
			}
		})
	}
}

// The value a recording keeps derives the same edge in a replay as the live value did: the
// connection name the volume field and the variable both carry pseudonymise identically.
func TestARecordedConnectionNameMatchesTheVolumeField(t *testing.T) {
	t.Parallel()
	key, _ := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	san, _ := sanitise.New(sanitise.ContractPolicy(), key)
	raw := []byte(`{"revisions":[{"containers":[{"env":[{"name":"INSTANCE_CONNECTION_NAME","value":"acme-prod:europe-west1:orders-db"}]}],` +
		`"volumes":[{"name":"cloudsql","cloudSqlInstance":{"instances":["acme-prod:europe-west1:orders-db"]}}]}]}`)
	prepared, err := gcpfeeder.RecordingPrepare(san)(gcpfeeder.PayloadRevisions, raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := san.JSON("gcp."+gcpfeeder.PayloadRevisions, prepared)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Revisions []struct {
			Containers []struct {
				Env []struct{ Value string } `json:"env"`
			} `json:"containers"`
			Volumes []struct {
				CloudSQLInstance struct{ Instances []string } `json:"cloudSqlInstance"`
			} `json:"volumes"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	env := decoded.Revisions[0].Containers[0].Env[0].Value
	volume := decoded.Revisions[0].Volumes[0].CloudSQLInstance.Instances[0]
	if env == "" || env != volume {
		t.Errorf("env %q and volume %q pseudonymise differently; the two derivations would name two instances", env, volume)
	}
}

// A recorded alert policy still watches what it watched live, in the recording's vocabulary — the
// thing a replayed investigation starts from — and nothing else of its filter survives: not a
// customer id in a metric label, not an OR branch, not a substring match, not a real name.
func TestARecordedAlertFilterKeepsItsWatchesAndNothingElse(t *testing.T) {
	t.Parallel()
	key, _ := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	san, _ := sanitise.New(sanitise.ContractPolicy(), key)
	const live = `resource.type = "cloud_run_revision" AND resource.labels.project_id = "acme-prod-123" AND ` +
		`resource.labels.location = "europe-west1" AND resource.labels.service_name = "canary-checkout-7f3" AND ` +
		`metric.type = "run.googleapis.com/request_count" AND metric.labels.customer_id = "cust-991" AND ` +
		`metric.labels.response_code_class = "5xx" AND (metric.labels.tenant = "other-svc" OR ` +
		`resource.labels.service_name != "x") AND resource.labels.revision_name = has_substring("v2")`
	sqlFilter := `resource.type = "cloudsql_database" AND resource.labels.database_id = "acme-prod-123:orders-db" ` +
		`AND resource.labels.region = "europe-west1"`
	raw, _ := json.Marshal(map[string]any{"alertPolicies": []any{map[string]any{
		"name": "projects/acme-prod-123/alertPolicies/42",
		"conditions": []any{
			map[string]any{"conditionThreshold": map[string]any{"filter": live, "comparison": "COMPARISON_GT"}},
			map[string]any{"conditionThreshold": map[string]any{"filter": sqlFilter}},
			map[string]any{"conditionMatchedLog": map[string]any{"filter": `jsonPayload.user = "alice@acme.example"`}},
			map[string]any{"conditionMatchedLog": map[string]any{"filter": `"Timed out talking to acme-billing"`}},
			map[string]any{"conditionThreshold": map[string]any{"filter": `metric.type="logging.googleapis.com/user/AcmeBillingErrors"`}},
			map[string]any{"conditionThreshold": map[string]any{"filter": `metric.type="monitoring.googleapis.com/uptime_check/check_passed" AND metric.label.check_id="acme-home-check"`}},
		},
	}}})
	prepared, err := gcpfeeder.RecordingPrepare(san)(gcpfeeder.PayloadAlertPolicies, raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := san.JSON("gcp."+gcpfeeder.PayloadAlertPolicies, prepared)
	if err != nil {
		t.Fatal(err)
	}
	recorded := string(out)
	for _, kept := range []string{"logging.googleapis.com/user/px_res_", "monitoring.googleapis.com/uptime_check/check_passed",
		`metric.label.check_id=\"px_res_`} {
		if !strings.Contains(recorded, kept) {
			t.Errorf("%q is missing from the recorded policy: %s", kept, recorded)
		}
	}
	if n := strings.Count(recorded, `"filter":`); n != 6 {
		t.Errorf("%d filters recorded, want all 6: a condition that loses its filter loses its pointer", n)
	}
	for _, leak := range []string{"acme", "Acme", "canary-checkout", "Timed out", "cust-991", "customer_id", "other-svc", "has_substring",
		"alice", "jsonPayload", " OR ", "orders-db"} {
		if strings.Contains(recorded, leak) {
			t.Errorf("%q survived in the recorded policy: %s", leak, recorded)
		}
	}

	// The structural half: fed to the feeder beside the recorded service, the recorded policy watches
	// the recorded service's ref and the recorded instance, exactly as the live one watched the live ones.
	project, _ := san.LabelIdentifier(sanitise.KindProject, "acme-prod-123")
	svc, _ := san.Identifier(sanitise.KindService, "canary-checkout-7f3")
	instance, _ := san.LabelIdentifier(sanitise.KindInstance, "orders-db")
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "twin",
		Scope:   gcpfeeder.Scope{Projects: []string{project}, Regions: []string{"europe-west1"}},
		Horizon: gcpfeeder.Horizon{Earliest: pollStart.Add(-24 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(f.Describe())
	payloads := []feeder.Payload{
		{Kind: gcpfeeder.PayloadAlertPolicies, At: pollStart, Bytes: out},
		{Kind: gcpfeeder.PayloadPollMarker, At: pollStart, Bytes: []byte(`{"outcome":"complete","projects":["` + project + `"],"regions":["europe-west1"]}`)},
	}
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), memory); err != nil {
		t.Fatal(err)
	}
	var watches []string
	for _, ev := range memory.Events() {
		if n := ev.GetUpsertNode(); n != nil && n.GetType() == graphv1.NodeType_ALERT {
			for _, v := range n.GetProps().GetFields()[gcpfeeder.PropAlertWatches].GetListValue().GetValues() {
				watches = append(watches, v.GetStringValue())
			}
		}
	}
	joined := strings.Join(watches, " ")
	wantService := gcpfeeder.Service{Project: project, Region: "europe-west1", Name: svc}.Ref().GetValue()
	if !strings.Contains(joined, wantService) {
		t.Errorf("the recorded policy does not watch the recorded service %q; watches: %v", wantService, watches)
	}
	if !strings.Contains(joined, instance) {
		t.Errorf("the recorded policy does not watch the recorded Cloud SQL instance %q; watches: %v", instance, watches)
	}
}
