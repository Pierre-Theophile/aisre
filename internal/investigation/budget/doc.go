// SPDX-License-Identifier: Apache-2.0

// Package budget holds the budget profiles, the admission check made before every call, the
// per-backend quota share, the synthesis reserve and the stop reasons (plan §The budget manager).
//
// Phase 1 creates the package; Phase 6 fills it in.
package budget
