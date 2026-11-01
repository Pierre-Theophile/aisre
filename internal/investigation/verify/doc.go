// SPDX-License-Identifier: Apache-2.0

// Package verify is the fresh-context verifier pass: every rendered claim is checked back
// against the evidence it cites before the answer is published (FR-022a).
//
// The engine runs both passes on every terminal path, before the stop record (FR-022c, T121).
package verify
