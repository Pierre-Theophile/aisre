// SPDX-License-Identifier: Apache-2.0

package chat

import (
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// Name is the worker's published name.
const Name = "chat"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// RecordedSource is the source of truth a chat worker over a recorded world declares: the world's
// digests are of a twin, not of any workspace a deployment could read.
const RecordedSource = "slack:recorded"

// New returns the chat worker over one backend: the live Slack backend in this package, or the
// recorded backend over a world of its digests. The worker cannot tell which, and that is what
// makes "live and recorded produce identical digests" a property of the worker (FR-034).
//
// sourceOfTruth is the one workspace the worker speaks for, "slack:<team id>" (FR-030).
func New(sourceOfTruth string, backend workers.Backend) *workers.Base {
	return &workers.Base{
		Declaration: worker.Description{
			Name:          Name,
			SourceOfTruth: sourceOfTruth,
			Capabilities: []worker.Capability{
				workers.Capability(engine.TermChatMentions),
				workers.Capability(engine.TermChatTimeline),
			},
			// No model reads a message: classes are matched by the published patterns.
			ContainsModel: false,
			Redaction:     workers.Redaction(),
			Modes:         workers.BothModes(),
			Version:       Version,
		},
		Backend: backend,
	}
}
