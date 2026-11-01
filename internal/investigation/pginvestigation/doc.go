// SPDX-License-Identifier: Apache-2.0

// Package pginvestigation is the investigation context's Postgres adapter, as the composition root
// sees it. It builds the runner's ports (runner.Persistence) over the investigation store and the
// projector, and adapts feature 001's query engine to the ports the graph and knowledge workers
// declare. Nothing outside the composition roots (internal/cli, internal/eval) and the tests
// imports it.
package pginvestigation
