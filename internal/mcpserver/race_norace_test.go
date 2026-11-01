// SPDX-License-Identifier: Apache-2.0

//go:build !race

package mcpserver_test

// raceDetectorEnabled is false in an ordinary build; see race_race_test.go.
const raceDetectorEnabled = false
