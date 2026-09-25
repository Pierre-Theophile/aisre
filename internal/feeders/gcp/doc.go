// SPDX-License-Identifier: Apache-2.0

// Package gcp is the feeder over the platform this organisation actually runs on.
//
// Its single responsibility is to turn what Google Cloud reports into typed, idempotent,
// replayable graph events: Cloud Run services and revisions, Cloud SQL instances, Cloud Run
// configuration, the Cloud Audit Logs admin-activity stream, Cloud Monitoring alert policies and
// their transitions, the GKE cluster's metadata, and — where the budget allows — load balancers
// and Cloud DNS. It writes nothing anywhere.
//
// It reads no GKE workload and creates no node the Kubernetes feeder owns (FR-030): in this
// organisation the deployed unit is a Cloud Run revision, and the one inference cluster stays with
// the feature 001 Kubernetes feeder.
//
// Contract: specs/003-gcp-integration/contracts/gcp-feeder.md. Entity shapes, refs, valid-time
// rules and the two state machines are specs/003-gcp-integration/data-model.md; quota is
// contracts/budget.md; every Google call goes through internal/gcpx.
package gcp
