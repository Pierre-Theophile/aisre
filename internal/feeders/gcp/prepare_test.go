// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
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
	out, err := gcpfeeder.PreparePayload(gcpfeeder.PayloadPollMarker, raw)
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
	if got, _ := gcpfeeder.PreparePayload(gcpfeeder.PayloadServices, other); string(got) != string(other) {
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
			prepared, err := gcpfeeder.PreparePayload(gcpfeeder.PayloadRevisions, raw)
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
	prepared, err := gcpfeeder.PreparePayload(gcpfeeder.PayloadRevisions, raw)
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
