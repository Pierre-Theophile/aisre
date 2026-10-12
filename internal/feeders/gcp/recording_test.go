// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// A live recording, end to end through what `feed gcp --record` builds (T187, FR-137): the poller,
// the GCP pre-pass, the contract table and the shadow feeder that derives the events. Identifiers are
// planted in the shapes a real project uses, and none may reach disk.
func TestALiveRecordingLeavesNoIdentifierOnDisk(t *testing.T) {
	t.Parallel()
	rig := newRig()
	svc := service("checkout")
	svc.Creator = "alice@example.com"
	// uid is kept verbatim because GCP mints it as a random UUID; the rig's readable uids are not.
	svc.Uid = "6f1c2a9e-3b7d-4e0a-9c1f-2d8b5e7a4c30"
	svc.Template = &runpb.RevisionTemplate{
		ServiceAccount: "checkout-runner@" + pollProject + ".iam.gserviceaccount.com",
		Containers: []*runpb.Container{{
			Image: "eu.gcr.io/" + pollProject + "/checkout@sha256:" + strings.Repeat("ab", 32),
			Env:   []*runpb.EnvVar{{Name: "DATABASE_PASSWORD", Values: &runpb.EnvVar_Value{Value: "hunter2-secret"}}},
		}},
	}
	rig.run.services = []*runpb.Service{svc}
	rev := revision("checkout", "checkout-00001-abc")
	rev.Uid = "0b9d7e1a-5c2f-4a8e-b3d6-9f1e0c7a2b45"
	rig.run.revisions = []*runpb.Revision{rev}
	rig.run.revisionsErr = nil
	rig.logs.pages = []auditPage{{entries: []*loggingpb.LogEntry{auditEntry(t, "e1", pollStart.Add(-time.Minute))}}}
	rig.mon.errs = []error{context.DeadlineExceeded} // a partial cycle, so the reason is recorded too

	key, err := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "recording")
	tee, err := deployrecord.NewTee(rig.poller(t, 1), "gcp", san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee = tee.WithPrepare(gcpfeeder.RecordingPrepare(san))
	drain(t, tee)
	if dropped := tee.Dropped(); len(dropped) > 0 {
		t.Fatalf("payloads were refused rather than recorded: %v", dropped)
	}

	pseudoProject, err := san.LabelIdentifier(sanitise.KindProject, pollProject)
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "twin",
		Scope:   gcpfeeder.Scope{Projects: []string{pseudoProject}, Regions: []string{pollRegion}},
		Horizon: gcpfeeder.Horizon{Earliest: pollStart.Add(-30 * 24 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := tee.Finish(t.Context(), dir, "gcp-topology", shadow)
	if err != nil || events == 0 {
		t.Fatalf("the recording's events were not derived (%d): %v", events, err)
	}

	var all strings.Builder
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		all.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	written := all.String()
	for _, secret := range []string{pollProject, "checkout", "alice", "hunter2", "checkout-runner"} {
		if i := strings.Index(written, secret); i >= 0 {
			t.Errorf("%q reached disk in the recording: …%s…", secret, written[max(0, i-80):min(len(written), i+40)])
		}
	}
	for _, kept := range []string{pseudoProject, pollRegion, "DATABASE_PASSWORD", "sha256:"} {
		if !strings.Contains(written, kept) {
			t.Errorf("%q is missing: a recording that keeps nothing replays to nothing", kept)
		}
	}
}
