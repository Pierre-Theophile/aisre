// SPDX-License-Identifier: Apache-2.0

//go:build race

package mcpserver_test

// raceDetectorEnabled is true under `go test -race`. TestOverheadBudget measures wall-clock time
// against SC-004's budget, and the detector instruments every memory access of the JSON work the
// adapter does, so under it the test measures the detector as much as the adapter.
const raceDetectorEnabled = true
