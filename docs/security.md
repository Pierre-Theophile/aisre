<!-- SPDX-License-Identifier: Apache-2.0 -->

# Security model

What this system is allowed to do, what an attacker gets by compromising each part of it, and
what the code does about it. For reporting a vulnerability, see [`SECURITY.md`](../SECURITY.md);
this page is the model the code is written against.

The first thing to say is the thing that shapes everything else: **sre-agent never writes to a
production system** (constitution VII). It reads topology and change history, and it writes only
to its own event log. There is no remediation path, no `kubectl apply`, no API call that changes
anything outside this database. An attacker who owns the whole system owns a very good map of
your production estate and cannot use it to touch that estate.

## Trust boundaries

```
  source systems                sre-agent                        callers
  ──────────────                ─────────                        ───────
  Kubernetes API  ──read-only──▶  feeder ──feeder token──▶ serve ◀──reader/decider token── CLI, agent
  OTLP receiver   ──push───────▶  feeder        (gRPC)       │
  (vendor APIs)                                              ▼
                                                        PostgreSQL
                                                     log.*   graph.*
```

Four boundaries, each with its own credential:

1. **Source system → feeder.** A read-only credential belonging to the source system (a
   Kubernetes ServiceAccount with `get,list,watch`, a vendor API key with a read scope). The
   feeder declares what it needs in `Description.RequiredScopes` and must refuse to start if its
   credential can write (FR-046). The SDK cannot verify this — only the source system knows what
   a scope grants — so it is a rule with a documented check, not an enforced one.
2. **Feeder → graph.** A bearer token with the `feeder` role and a `source_id` claim. Scoped to
   exactly one source. No database credential.
3. **Caller → graph.** A bearer token with the `reader` or `decider` role, naming an individual
   (FR-041a). No shared credentials: a decision from a shared identity is refused, which is what
   SC-011 measures.
4. **Graph → PostgreSQL.** A database role with no `DELETE` and no `TRUNCATE`. A query-only
   deployment gets a `SELECT`-only role. See [`../deploy/README.md`](../deploy/README.md#database-roles-least-privilege-per-process).

The health endpoints (`/healthz`, `/readyz`) are the one unauthenticated surface. A load
balancer has no token, and a liveness probe that needed one would fail closed on an
identity-provider outage. They answer `ok`, `ready`, `database unavailable` or `schema not
migrated` — four fixed strings. No DSN, no host name, no driver error, no version.

## Roles

Three roles, and the taxonomy is closed (research §7):

| Role | May | May not |
|---|---|---|
| `reader` | run every query | ingest; record a decision |
| `decider` | everything `reader` may, plus record resolution decisions | ingest |
| `feeder` | ingest events **for the one source its token names** | query; decide |

`decider` implies `reader` — a decision is meaningless without seeing what is being decided.
`feeder` implies nothing at all: a feeder cannot read the graph it writes to. Authentication is
an interceptor; authorization is each handler's own `Require` or `RequireSource` call, because
"read-only credentials suffice for every query" and "a feeder may only write for its own source"
are different rules and conflating them is how one of them gets forgotten.

## What a compromised feeder token gets you

This is the credential most likely to leak: it lives in a cluster, in a Secret, on a machine you
do not fully control.

**It can:** append events for exactly one `source_id` — invent workloads, invent edges, invent
change nodes, retract real ones, and emit identity claims that argue two entities are the same.
That is enough to poison what the graph believes about that source's part of the estate, and
therefore to mislead an investigation.

**It cannot:**

- **Write for another source.** Every event is checked against the token's `source_id` before
  anything is applied, on `Ingest`, `IngestBatch` and `RegisterSource` alike. A k8s feeder token
  cannot forge an OTel observation, so a lie is always attributable to one source.
- **Read the graph.** `feeder` does not imply `reader`. The token is not an exfiltration path
  for your topology.
- **Merge two entities.** Merging is a decision; decisions need `decider`, and a claim from a
  probable rule becomes a suggestion for a human, never an automated merge (constitution VI).
- **Delete or rewrite history.** There is no such API and no such grant. A retraction closes a
  valid interval and a correction opens a new observed interval; the original rows stay, and
  `query history` still shows them. The forgery is *visible*, attributable and reversible.
- **Reach the database.** Feeders hold no DSN.
- **Touch production.** Nothing here can.
- **Probe another source's idempotency keys.** The scope check runs before the event id is even
  logged, so a rejected event leaks nothing about what the graph already holds.

The recovery from a leaked feeder token is therefore: revoke it at the identity provider, and
query the graph for what that source asserted while it was valid — which is a question the
bitemporal model can actually answer, because "what did we believe, and when did we learn it"
is the thing it is built to store.

## Findings and fixes (T090, 2026-09-17)

The security pass reviewed authentication, the dev provider, feeder scoping, credential handling
and the fixture corpus. Everything below has a test; the test name is the guarantee.

| # | Finding | Severity | Fix | Test |
|---|---|---|---|---|
| 1 | `--token`'s default was `os.Getenv("SRE_AGENT_TOKEN")`. Cobra renders a flag's default into `--help`, so `aisre --help` printed the caller's live bearer token — to a terminal, a CI log, a screenshot on a ticket. | **High** | The flag defaults to empty; the environment is read in `PersistentPreRunE` (`tokenFrom`). | `cli.TestHelpNeverPrintsTheToken`, `cli.TestTokenFlagHasNoDefaultValue`, `cli.TestTokenFrom` |
| 2 | `serve --auth dev --dev` bound any address, including `:8080`. The dev signing key is a published constant, so a dev server on a routable address is a credential-minting service: anyone who can reach the port mints `alice` with every role. | **High** | Loopback only, unless `--dev-insecure-listen` is passed. A `--dev-users` file does not lift it: the file restricts *who* may be minted, not who may mint. | `cli.TestServeAuthFailsClosed`, `cli.TestIsLoopbackListen` |
| 3 | The OIDC audience check could only be satisfied, never consciously skipped: `--oidc-audience` was mandatory, so an operator whose provider scopes tokens differently had no supported path and would have reached for a wildcard audience. | Medium | `--oidc-skip-audience` exists, is explicit, logs a warning naming the risk, and contradicts `--oidc-audience` rather than silently losing to it. Absent both, the server refuses to start. | `cli.TestServeAuthFailsClosed`, `server.TestOIDCRequiresAudienceOrExplicitSkip` |
| 4 | `--oidc-issuer` accepted `http://`. Discovery and the JWKS travel over that URL, so anyone on the path could serve their own key set and mint any identity. | **High** | `https` required; `http` allowed only with `--dev`, for a local mock provider. | `cli.TestCheckIssuerTLS`, `cli.TestServeAuthFailsClosed` |
| 5 | Token expiry and `nbf` were enforced by go-oidc but asserted nowhere, so a dependency bump could have removed the enforcement silently. | Low | Assertions, not a comment. | `server.TestOIDCNotYetValidTokenIsRejected`, `server.TestOIDCTokenWithoutExpiryIsRejected` |
| 6 | `RequireSource` was tested on `IngestBatch` and `RegisterSource` but not on the `Ingest` stream — the path a live feeder actually uses, and the one where the check is easiest to forget, because the interceptor runs once while the source id arrives per message. | Medium | The check was already correct; it is now pinned on all three doors, for both the wrong role and the wrong source. | `server.TestReaderTokenCannotIngest`, `server.TestIngestStreamForAnotherSourceIsPermissionDenied`, `server.TestRegisterSourceIsScopedToTheToken` |
| 7 | Role separation was asserted piecemeal. | Low | One table: reader cannot ingest, decider cannot ingest, feeder cannot query, feeder cannot decide, feeder cannot write for another source — plus the two cases that must succeed, so the test is not vacuously green. | `server.TestRoleSeparation` |
| 8 | A graph base URL written `https://feeder:secret@graph.internal` put the credential into every emitter error message, log line and client span naming the endpoint. | Medium | `normalizeBaseURL` strips userinfo; URL parse errors no longer quote the URL back. | `emit.TestConnectEmitterStripsCredentialsFromTheBaseURL` |
| 9 | Nothing logged the bearer token, but nothing stopped the next person either: a struct dump of the transport or a debug line printing headers would have. | Low | `bearerTransport` implements `slog.LogValuer` and `String()` as `[redacted]`; `emit.RedactAuthorization` is the supported way for a feeder to log its own headers, and covers `Proxy-Authorization` and cookies too. | `emit.TestRedactAuthorization`, `emit.TestConnectEmitterNeverLogsTheToken` |
| 10 | Nothing stopped a secret entering the repository through a recording. Fixtures are recordings of real clusters, and the graph's own `ReasonSecretValue` refusal does not cover a payload that never reaches the graph. | Medium | `scripts/check-no-secrets.sh`, wired into the `lint` job of `ci.yml`. | the script's own `UPDATE_BASELINE` round trip, and CI |
| 11 | No documented read-only database role, so "read-only credentials suffice for every query" (FR-035) was a property of the code alone. | Low | `deploy/README.md` documents the `SELECT`-only role with its `GRANT`s, the `serve` role with no `DELETE`/`TRUNCATE`, and a one-line proof that a write fails. Verified against PostgreSQL 16. | manual, recorded in `docs/benchmarks/quickstart-run-2026-09-17.md` |

Reviewed and found already correct, so recorded rather than changed:

- The dev provider refuses to build without `--dev`, in **both** halves — the verifier in
  `serve` and the minter in `dev-token` — so a flag typo cannot hand out a credential a correct
  server would have refused (`server.TestDevProviderAndIssuerBothRefuseWithoutDev`).
- The dev banner names the issuer, the audience, the users file and whether the published
  default key is in use, and never prints the key
  (`server.TestDevBannerSaysWhenTheDefaultKeyIsInUse`).
- A dev issuer is `sre-agent-dev`, not a URL, so a dev principal key can never be mistaken for a
  real one in an audit trail.
- An authentication failure says only "bearer token is not valid". Which check failed is not
  disclosed: "wrong audience" would tell an attacker the signature verified, which is the
  expensive half of the guess (`server.TestOIDCRejectionDoesNotDiscloseWhichCheckFailed`).
- `/healthz` and `/readyz` leak nothing about the database beyond up/down and migrated/not
  (`server.TestHealthEndpointsArePublic`).
- Authorization failures name the principal and the role, never the token
  (`server.TestAuthorizationErrorsCarryNoCredential`).

## The secrets scanner

`scripts/check-no-secrets.sh` scans `fixtures/**`, `internal/feeders/*/testdata/**` and
`deploy/**` — everything that holds recorded or deployed material. It is a grep, deliberately;
a scanner that is trusted and wrong is worse than a grep that is dumb and honest.

Two severities. **Hard rules** — a private key header, an `AKIA`-style cloud key, a JWT or a
bearer header, a plaintext Kubernetes `stringData`, a database password that is not the
published local one — always fail. **Baselined rules** — a Secret's base64 `data` value and
`kubectl`'s `last-applied-configuration` annotation — fail unless that exact occurrence is
listed in `scripts/known-recording-findings.txt` by file and by digest of the matched text,
never by value. The baseline is what a human has looked at and accepted; anything new fails.

Today's baseline holds the kind-demo recordings. Every Secret value in them decodes to one
placeholder digest, and the applied-configuration annotations carry the public demo overlay. A
single line may be exempted in place with a `check-no-secrets: allow` comment, which makes the
exemption a reviewable line in the file rather than a rule in the script.

Go sources are not scanned: `internal/feeders/k8s/informers_test.go` is the regression test for
the applied-configuration leak and has to contain the shape it tests for.

**Open**: the baselined findings are accepted, not eliminated. The applied-configuration
annotation is never needed by a fixture and the recorder should strip it at record time, which
would empty most of the baseline. Tracked for the next fixture re-record.

## Things this model does not cover

- **Transport.** The server speaks h2c: TLS is terminated by your ingress. On a laptop that is
  fine; in a cluster, do not expose the port without one.
- **Rate limiting and quotas.** A feeder token with the right scope can fill the log. There is
  no quota, and the mitigation today is that the log is append-only and attributable.
- **The identity provider itself.** If your IdP mints a token with the `decider` role for the
  wrong person, the graph believes it. Role mapping is `OIDCConfig.RoleMapping`, and it is
  yours to get right.
- **Confidentiality of the graph.** Anyone with `reader` sees the whole graph. There is no
  per-node authorization, and adding one would be a design change, not a configuration.
