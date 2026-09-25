// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// `version`.
//
// The revision comes from the build info the Go toolchain stamps in, so a binary built from a
// dirty tree says so. An operator comparing a golden fixture against a live graph needs to
// know exactly which build produced it (constitution VIII), and "dev" with no revision is a
// meaningful answer: it says "this came off somebody's laptop".

func newVersionCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version, revision and toolchain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			revision, modified := buildRevision()

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(struct {
					Version  string `json:"version"`
					Revision string `json:"revision"`
					Modified bool   `json:"modified"`
					Go       string `json:"go"`
					Platform string `json:"platform"`
				}{
					Version:  version,
					Revision: revision,
					Modified: modified,
					Go:       runtime.Version(),
					Platform: runtime.GOOS + "/" + runtime.GOARCH,
				})
			}

			suffix := ""
			if modified {
				suffix = " (dirty)"
			}
			return p.writeTable(nil, [][]string{
				{"version", version},
				{"revision", revision + suffix},
				{"go", runtime.Version()},
				{"platform", runtime.GOOS + "/" + runtime.GOARCH},
			})
		},
	}
}

func buildRevision() (revision string, modified bool) {
	revision = "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return revision, false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}
