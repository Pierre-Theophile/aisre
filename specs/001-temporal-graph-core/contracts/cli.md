# CLI Contract: `sre-agent` (working name)

Every command has `--output=table|json` (json = canonical serialization used by goldens),
`--server=<url>`, and authenticates with an OIDC token from `$SRE_AGENT_TOKEN` or the login
cache. `--as-of` and `--observed-at` accept RFC 3339 or relative (`-30m`).

## Server and operations
| command | purpose | FR |
|---|---|---|
| `serve --db=<dsn> --listen=:8080 --auth=oidc\|dev [--dev]` | run API + ingestion + resolution | all |
| `migrate --db=<dsn>` | apply embedded schema migrations (refuses history-dropping migrations) | III |
| `extent` | earliest/latest observed, per-source checkpoints and gaps | FR-052 |

## Feeders
| command | purpose | FR |
|---|---|---|
| `feed otel --listen-grpc=:4317 --listen-http=:4318 --window=5m --source-id=otel:x [--record=<dir>] [--replay=<dir>]` | OTLP receiver → topology events | FR-042, FR-044 |
| `feed k8s --kubeconfig=... --context=... --source-id=k8s:prod [--owner-labels=team,owner] [--commit-labels=<key>,...] [--record=<dir>] [--replay=<dir>]` | informers → workload/config/change events; `--commit-labels` (004 T149, opt-in, default none) names the pod-template keys a rollout's `deploy.commit_sha` is read from | FR-043, FR-044, FR-046 |

`--record` writes raw payloads and emitted events (fixture-format.md). `--replay` reads
payloads and runs the same handler; the server is optional (`--dry-run` prints events).

## Queries
| command | maps to | FR |
|---|---|---|
| `query subgraph <ns>=<value> --as-of T [--observed-at O] --hops 2 --direction both [--edge-types calls,depends-on] [--min-weight 2]` | `QueryService.Subgraph` | FR-026 |
| `query diff <ns>=<value> (--from T1 --to T2 \| --at A [--window 2h]) [--observed-at O] [--hops 2] [--reference T] [--tau 30m]` — `--at` is an alert instant: the window is the `--window` before it and the ranking is measured to it; every ranked change is printed with its actor, actor kind, commit, rollback flag and a link that opens the run (004 SC-016) | `QueryService.Diff` | FR-027, FR-028, 004 SC-016 |
| `query impact <ns>=<value> --as-of T [--observed-at O] [--max-hops N] [--total-cap N]` | `QueryService.Impact` | FR-029 |
| `query pointers <ns>=<value> --as-of T` | `QueryService.Pointers` | FR-030 |
| `query history <ns>=<value>` | `QueryService.NodeHistory` | FR-032 |

Focus refs are written `otel.service.name=checkout`, `k8s.deployment=prod/checkout-svc`, or
a bare canonical `id:<entity_id>`.

## Resolution
| command | maps to | FR |
|---|---|---|
| `resolve suggestions [--for <ref>] [--status pending]` | `Suggestions` | FR-033 |
| `resolve why <refA> <refB>` | `ResolutionAudit` | FR-031 |
| `resolve confirm <refA> <refB> --reason "..."` | `ResolutionService.Confirm` | FR-040 |
| `resolve reject <refA> <refB> --reason "..."` | `Reject` | FR-040 |
| `resolve merge <refA> <refB> --reason "..."` | `Merge` | FR-040 |
| `resolve split <ref> --detach <ns>=<value>[,...] --reason "..."` | `Split` | FR-039 |

All resolution commands require the `decider` role; the server stamps the authenticated
principal (FR-041). They fail with exit code 3 if the token is anonymous or shared.

## Fixtures
| command | purpose | FR |
|---|---|---|
| `fixture verify <dir>... [--skip-goldens] [--report] [--report-json <file>]` | replay from empty DB, compare goldens (`golden/pinned/` is required, not optional), check `expect_rejected`, double-deliver, shuffle within window; exit 0 only if all pass; `--skip-goldens` for fixtures without goldens yet; `--report` emits ranking, calibration and resolution metrics; `--report-json` also writes each fixture's canonical report to a file, one per line, so CI can publish the human summary and gate on the numbers from one run (`scripts/check-report.sh`) | FR-047, FR-048, SC-003..005 |
| `fixture load <dir>` | apply `events.jsonl` (with recorded observed times) into the running server; used by quickstart | FR-047 |
| `dev-token --user <name> --roles r,d` | mint a dev-provider token (requires server `--auth dev --dev`) | research §7 |
| `fixture record <dir>` | regenerate `events.jsonl` and `golden/` from `payloads/` + `manifest.yaml` | FR-049 |
| `fixture export --manifest <path> --out <dir> [--compare <fixture-dir>]` | run a manifest's queries against the database `--db` names, as it stands, and write canonical answers laid out like `golden/` (plus `pinned/`), so `diff -r <out> <fixture>/golden` is the live-versus-replayed comparison; creates no database and replays nothing; `--compare` makes the comparison in-process and exits 4 when the two differ | SC-010 |
| `fixture assemble --out <dir> <recording-dir>...` | merge several `--record` directories into one fixture: payloads renumbered per kind and re-indexed by arrival, events ordered by observed time with `appendedSeq` renumbered, manifest carrying the union of the runs' declared sources | FR-050 (b) |
| `fixture bench [--scale 10k\|1k] [--seed N] [--queries 1000] [--out DIR] [--skip-replay] [--keep-db] [--note "..."]` | generate the reference workload (power-law topology, change nodes, two-source identity claims that merge ~30% of services) into databases of its own, measure p50/p95/p99 over the query families of research §2 plus ingestion throughput and full replay wall time, and write `<date>-scale-<scale>.md` and `.json`; exit 4 when SC-001, SC-002 or an ADR-0002 exit criterion is missed | research §2, SC-001, SC-002 |

## Exit codes
0 ok · 1 usage · 2 server/transport error · 3 authentication/authorization · 4 verification
failed (fixtures) · 5 rejected event (feeders, dry-run)
