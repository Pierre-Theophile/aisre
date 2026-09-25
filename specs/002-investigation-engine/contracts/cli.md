# CLI Contract: the investigation commands

Extends [001's CLI contract](../../001-temporal-graph-core/contracts/cli.md) and inherits its
conventions: `--output=table|json` (json = the canonical serialization goldens use), `--server`,
OIDC token from `$SRE_AGENT_TOKEN`, `--as-of`/`--observed-at` accepting RFC 3339 or relative
(`-30m`), and the exit codes `0 ok · 1 usage · 2 transport · 3 auth · 4 verification`.

Every command below produces **both** renderings from the same run (FR-064): the human form
ordered verdict → ranked list with exonerations → timeline → narrative, and the machine form with
stable field names containing no claim absent from the other.

## Investigate

| command | purpose | FR |
|---|---|---|
| `investigate <ns>=<value> --at T [--lookback 90m] [--profile page\|review] [--review-mode] [--observed-at O] [--watch]` | run an investigation on a node reference and instant | FR-001, FR-004, FR-005 |
| `investigate --alert <file>\|- [--profile …]` | run from a normalised alert intake on stdin or a file | FR-001, FR-002 |
| `investigate declare --severity <s> --at T --title "…" --origin <system>:<stable-id> [--service <ns>=<v>]… [--lookback 90m]` | a **human-declared** incident: a first-class intake, no monitor required. Observed time is pinned to the declaration instant; the idempotency key is (source, channel-or-incident id, declared instant), so re-delivery is a no-op returning the same investigation | FR-001a, FR-002a, FR-002b, FR-004a, FR-008b |
| `investigate report <id> [--deliver] [--sink <name>] [--db <dsn>]` | render the report for the place the incident lives and, with `--deliver`, update it **in place**; report-only, non-blocking, and every delivery outcome — delivered, failed or not-configured — is recorded in `investigation.report_deliveries` (one row per investigation and target, `update_count` incremented on each edit) without preventing conclusion. `--db` defaults to `$PG_DSN`; with neither, the report is delivered and the attempt is not recorded, and the command says so | FR-057f |
| `investigate get <id> [--evidence] [--ledger] [--chain]` | read an investigation; `--chain` prints the full evidence chain in the order produced | FR-053 |
| `investigate list [--incident <id>] [--status running] [--since T]` | list investigations | FR-046 |
| `investigate watch <id>` | follow an in-flight investigation: calls made so far and spend against each budget | FR-046 |
| `investigate export <id> --out <dir>` | write the self-contained artifact: decision record, ledger, both recording layers, the graph events its queries need | FR-042 |
| `investigate replay --from <dir> [--layer trajectory\|world] [--report-json <file>]` | replay with no network; exit 4 on divergence, naming the first diverging record | FR-039, FR-040, FR-041 |
| `investigate fact <id> --kind <k> --statement "…" [--entity <ns>=<v>]… [--from T] [--to T]` | push a typed human fact; never blocks; reopens a concluded investigation as a linked record | FR-057a, FR-057b |
| `investigate review <id> --root-cause <change-ref>\|unobserved\|not_change_induced:<cat> [--amend <hyp>=<status>]… --reason "…"` | record a human review; additive, attributable, survives replay | FR-054 |
| `investigate label <id> --right\|--wrong` | the one-click "was this right?" | FR-057e |
| `investigate to-incident <id> --out fixtures/incidents/<id>` | turn a reviewed investigation into a corpus incident with no hand-editing | FR-055, SC-012 |

`--watch` on `investigate` streams the anytime shape: the provisional prior-only ranking within
seconds, labelled provisional and untested, then the first wave, then each turn.

## Workers and backends

| command | purpose | FR |
|---|---|---|
| `worker list` | registered workers with their source of truth, capabilities, whether they contain a model, redaction and modes | FR-010 |
| `worker call <worker> <term> --args <json> [--mode live\|recorded] [--recording <dir>]` | issue one algebra term by hand and print the digest; the way a human re-runs an evidence item | FR-057d |
| `worker record --out <dir> --focus <ns>=<v> --window T1..T2 [--hops 2] [--grid <spec>]` | record a **world**: the cross product of the telemetry algebra over the neighbourhood and window grid, plus depth-1 drill-downs | FR-042b |
| `backend list` | registered telemetry backends, their terms, cost classes (`cheap\|standard\|expensive`) and quota reporting | FR-047a |
| `knowledge link <doc-ref> --kind <postmortem\|runbook\|decision\|investigation> --entity <ns>=<v>… [--as-of T]` | register a durable document a human wrote as a `KNOWLEDGE_DOC` node with a `CONCERNS` edge per entity and a pointer to where it lives — **never its content**. The v1 knowledge corpus is past investigations plus documents registered this way | FR-049a, FR-052 |

## The coverage audit

| command | purpose | FR |
|---|---|---|
| `audit coverage --incidents <file> [--out docs/evaluation/] [--graph-config <name>]` | classify each supplied incident as cause present / cause absent with a category / undecidable; publish the ceiling with its incident count, the per-category counts and the feeder set measured against | FR-069, FR-070, FR-071 |
| `audit coverage compare <a.json> <b.json>` | compare two audits so a feeder's effect on the ceiling is measurable rather than asserted | FR-071 |

The audit takes a human-supplied incident list and **never infers a cause**. Its ceiling is the
prior of *no observed change explains this* (research §8) and the upper bound on every published
accuracy target.

## Fixtures and evaluation (extends 001's `fixture` commands)

| command | purpose | FR |
|---|---|---|
| `fixture verify <dir>… --report [--report-json <file>]` | **unchanged**: 001's ranking and resolution metrics over a fixture directory. The world-replay evaluation is `eval run` below, not a flag on this command — see §Why the evaluation is its own verb | FR-058, FR-059, FR-063 |
| `fixture verify <dir>… --trajectory-only [--report-json <file>]` | **what `ci.yml` runs over incident fixtures on every pull request**: everything 001 already does to the directory, plus byte-identical replay of every recorded trajectory, with zero network and **zero model calls**. It computes calibration from the recorded trajectories, whose confidences are ledger-computed and therefore deterministic. The k-run world replay above is **`eval.yml`'s job**, not CI's | FR-042a, FR-058, SC-003 |
| `fixture derive <dir> --transform culprit-deleted\|decoy-injected\|time-shifted\|name-permuted --out <dir>` | multiply the corpus mechanically, recording provenance, the parent and the transformation, and the invariant the variant is graded on | FR-062a |
| `fixture record-trajectory <dir> [--model-free] [--fake-model] [--live] [--model-config <file>] [--prices <file>]` | record an incident fixture's layer-1 trajectory: the investigation its `incident:` block states, against the fixture's own graph and recorded `world/`. With no kind flag it records the two network-free runs the corpus ships — the deterministic `model-free` one and the `fake-model` one whose canned turns exercise the model-request digest matching path. `--live` is **additive, never implied**: it calls the providers `--model-config` names and writes a trajectory whose model exchanges are real turns, and the other recordings stay — the fake-model run is what every pull request replays with no credential in the environment, and the live run is the production-configuration recording. A live run with no credential for a configured provider exits 2 naming the variable. The run id is derived from the trajectory's digest, so re-recording an unchanged fixture rewrites the same bytes | FR-042a, FR-061, FR-063 |
| `fixture record-world <dir>` | (re)record the `world/` layer of an incident fixture, reading the focus, window grid, hop radius and drill-down depth from **that fixture's own manifest** rather than from flags, so a re-record cannot silently change the fixture's shape; a re-record of an unchanged fixture is byte-identical. Implemented alongside `worker record` (T039) | FR-042b |

### Why the evaluation is its own verb (T109, decided 2026-09-18)

`fixture verify --report` was going to carry the investigation rows, and it does not. The split is
by **cadence**, and each command keeps one promise:

| command | cadence | needs |
|---|---|---|
| `fixture verify … --trajectory-only` | every pull request | no database, no egress, no model |
| `eval run …` | nightly and on demand | a database per fixture, the corpus × k, and with a live configuration, money and minutes |

Hanging the world-replay evaluation off `fixture verify` would have produced one of two bad
outcomes: the per-pull-request gate slows to the evaluation's pace, or the evaluation hides behind
a flag on a command whose documented promise is "this is cheap". `fixture verify` therefore keeps
doing exactly what 001 made it do, and still runs `--trajectory-only` with no runs at all.

| command | purpose | FR |
|---|---|---|
| `eval run --fixtures <dir> [--runs 3] [--extend-to 7] [--corpus public\|private] [--model-free\|--fake-model\|--live] [--batch] [--report-json <file>] [--summary <file>] [--doc <file>]` | the evaluation job: the corpus × k in world-replay mode, graded against each fixture's ground truth, published as one `{metric, scope, fixture, value, n, detail}` row per line. `--doc` regenerates the tables of `docs/evaluation/investigation-metrics.md` between its generated markers and leaves its prose alone. `--model-free` makes no model call and labels every number it publishes. `--live` calls the providers `--model-config` names for real; without a credential for one of them the run **falls back to model-free and says which variable was missing**, rather than dying on fixture 3 of 16. `--batch` is **reserved**: `internal/investigation/model` has no Batch API client yet, so the flag is accepted, reported and does nothing | FR-058, FR-059, FR-060, FR-061, FR-067 |
| `eval power --n <trials> [--baseline <rate>] [--direction min\|max] [--zero-tolerance] [--quiet]` | what regression a gate of n Bernoulli trials can actually detect, with the method printed beside the answer. It is a command rather than arithmetic in `check-report.sh` because the statement needs a square root, a normal quantile and a bisection, and a second copy of that in `awk` would be a second, divergent definition of a rule the project states once | FR-060, FR-061 |

`scripts/check-report.sh --investigation <rows.jsonl>` is the gate over those rows; `--investigation
--set-threshold <rows.jsonl> [--force]` publishes the aggregate pass@1 threshold from the first full
corpus run, once, by hand (FR-060), refusing any value above the coverage ceiling of the audit it
cites (FR-071).

`fixture verify` exits 4 when a gated metric fails, and prints the gate it failed **together with
the regression that gate can actually detect** (FR-061).

## Serving

`serve` gains `--enable-investigation`, `--model-config <file>`, `--recording-root <dir>` and
`--budget-profiles <file>`. Without `--enable-investigation` the engine is absent and the binary
behaves exactly as it does today.

**Credentials follow the providers the configuration names**, not a single vendor: each role in
`--model-config` declares `provider: anthropic | mistral`, and the credential is
`$ANTHROPIC_API_KEY` or `$MISTRAL_API_KEY` accordingly. A configuration naming both needs both.
`serve` checks at startup and, when one is missing, logs a warning naming the variable and runs
**model-free** — the prior-only ranking, the causal ordering and the deterministic first wave
(FR-067) — rather than failing on its first turn in the middle of an incident. The same rule is
what `eval run --live` falls back on and what `fixture record-trajectory --live` refuses on.

## Refusals that are not errors

- A worker asked something outside the published algebra exits 1 with
  `outside_algebra`, naming what was asked and what is available.
- `investigate` started with a write-scoped credential exits 3 (FR-008).
- An investigation with an anonymous or shared credential exits 3 (FR-066).
