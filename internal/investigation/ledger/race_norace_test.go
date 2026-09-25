// SPDX-License-Identifier: Apache-2.0

//go:build !race

package ledger_test

// raceDetectorEnabled is false in an ordinary build. See race_race_test.go for the other half and
// for why the wall-clock budget assertion needs to know.
const raceDetectorEnabled = false
