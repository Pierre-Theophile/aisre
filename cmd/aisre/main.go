// SPDX-License-Identifier: Apache-2.0

// Command sre-agent is the single binary of the AI SRE agent: it serves the temporal system
// graph API, runs the reference feeders, and provides the query and fixture command line.
//
// One binary, no sidecars, no code generation at runtime (constitution X). Everything it does
// is a subcommand; `aisre --help` lists them.
package main

import "github.com/Pierre-Theophile/aisre/internal/cli"

// version is the release version, overridden at link time with
// -ldflags "-X main.version=<tag>". "dev" means an untagged local build.
var version = "dev"

func main() {
	cli.SetVersion(version)
	cli.Execute()
}
