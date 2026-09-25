// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/server"
)

// `dev-token` (contracts/cli.md §Fixtures, research §7).
//
// Fixtures, CI and laptops need a credential without an identity provider. The dev provider
// signs one locally with a published key, which is precisely why both halves of it — minting
// here and verifying in `serve` — refuse to work without `--dev`. A single flag typo must
// never be enough to hand out credentials a correctly configured server would have refused,
// and a token minted this way must be recognizable as such: its issuer is `sre-agent-dev`, not
// a URL, so a dev principal key can never be confused with a real one.

type devTokenOptions struct {
	user      string
	roles     string
	usersFile string
	sourceID  string
	dev       bool
}

func newDevTokenCommand(global *globalOptions) *cobra.Command {
	opts := &devTokenOptions{}

	cmd := &cobra.Command{
		Use:   "dev-token",
		Short: "Mint a token for the local dev identity provider",
		Long: "dev-token signs a bearer token that a server running with `--auth dev --dev` accepts.\n" +
			"It requires --dev here too: these tokens are not a security boundary and must never be\n" +
			"mintable by accident.\n\n" +
			"Roles are reader, decider, feeder and investigator, or their aliases r, d, f and i.\n" +
			"A feeder token must be scoped to one source, with --source-id or through --users;\n" +
			"`investigator` is what the investigation RPCs need (002 FR-064) and it implies reader.\n\n" +
			"  export SRE_AGENT_TOKEN=$(aisre dev-token --dev --user alice --roles r,d)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDevToken(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.user, "user", "", "user name to mint a token for (required)")
	flags.StringVar(&opts.roles, "roles", "", "comma-separated roles, e.g. r,d or reader,decider")
	flags.StringVar(&opts.usersFile, "users", "",
		"YAML or JSON file of dev users; when set, only its users and roles may be minted")
	flags.StringVar(&opts.sourceID, "source-id", "",
		"source a feeder token is scoped to, e.g. otel:demo (feeder role only, no --users)")
	flags.BoolVar(&opts.dev, "dev", false, "confirm that dev credentials are wanted (required)")

	return cmd
}

func runDevToken(cmd *cobra.Command, global *globalOptions, opts *devTokenOptions) error {
	if !opts.dev {
		return exitErrorf(ExitUsage,
			"dev-token needs --dev: %v", server.ErrDevAuthDisabled)
	}
	if opts.user == "" {
		return exitErrorf(ExitUsage, "dev-token needs --user")
	}

	roles, err := server.ParseRoles(opts.roles)
	if err != nil {
		return exitErrorf(ExitUsage, "--roles %q: %v", opts.roles, err)
	}

	issuer, err := server.NewDevIssuer(server.DevConfig{
		Enabled:   true,
		UsersFile: opts.usersFile,
		// The banner belongs to `serve`; minting writes only the token to stdout so that it
		// can be captured by a shell.
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	var token string
	switch {
	case opts.usersFile != "":
		if opts.sourceID != "" {
			return exitErrorf(ExitUsage,
				"--source-id cannot be combined with --users: the users file is the single statement of who may feed what")
		}
		token, err = issuer.Mint(opts.user, roles)
	default:
		token, err = issuer.MintFor(server.DevUser{
			Name:     opts.user,
			Roles:    roles,
			SourceID: opts.sourceID,
		})
	}
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(struct {
			Token    string   `json:"token"`
			User     string   `json:"user"`
			Issuer   string   `json:"issuer"`
			Roles    []string `json:"roles"`
			SourceID string   `json:"source_id,omitempty"`
		}{
			Token:    token,
			User:     opts.user,
			Issuer:   server.DefaultDevIssuer,
			Roles:    roleNames(roles),
			SourceID: opts.sourceID,
		})
	}
	// Bare token on stdout: the documented use is `export SRE_AGENT_TOKEN=$(... dev-token ...)`.
	return p.writeLine("%s", token)
}

func roleNames(roles []server.Role) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, string(role))
	}
	return names
}
