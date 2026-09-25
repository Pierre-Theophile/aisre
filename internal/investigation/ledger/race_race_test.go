// SPDX-License-Identifier: Apache-2.0

//go:build race

package ledger_test

// raceDetectorEnabled is true under `go test -race`.
//
// It exists for one assertion: TestPosteriorsAreComputedWellInsideTheBudget measures wall-clock
// time against a published budget, and the race detector instruments every memory access, so under
// it the test measures the detector as much as the code.
//
// Both halves are `_test.go` files, so nothing about the race detector reaches the shipped binary.
const raceDetectorEnabled = true
