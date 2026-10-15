// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/storetest"
)

// TestConformance: the in-memory store passes the published suite unmodified (008 FR-007).
func TestConformance(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(*testing.T) knowledge.Store { return memory.New() })
}
