// SPDX-License-Identifier: Apache-2.0

// Package archtest enforces the dependency rule of the codebase: domain and SDK packages never
// reach infrastructure (a database driver, a vendor SDK, a cloud client), and a bounded context
// never reaches into another one's internals the wrong way round. The rules live in
// archtest_test.go and run with `go test ./...`, so a violation fails CI the moment it is
// introduced instead of being found in the next audit. docs/architecture/contexts.md is the map
// they enforce.
package archtest
