# Quickstart run — 2026-09-27

**Quickstart**: [quickstart.md](./quickstart.md) · **Task**: T098

## Setup

The run used a clean `git worktree` of the branch head, which carried T068, T093, T095 and T096. It
used a local PostgreSQL through `PG_DSN`, and `buf` and the protoc plugins installed with
`make tools`. Every command was run as written in the quickstart, from the worktree's root.

## Result

§0–§6 pass. Four places in the file were wrong, and each is fixed in [quickstart.md](./quickstart.md).

| § | Command | Result |
|---|---|---|
| 0 | `make build`; `bin/aisre migrate --db "$PG_DSN"` | built; 13 migrations applied |
| 1 | `buf lint && buf breaking --against '.git#branch=main'` | clean |
| 1 | `go test ./pkg/feeder/ -run 'ReadOnly\|NamedQuery'` | 4 pass |
| 1 | `go test ./pkg/feeder/ -run 'Quota.*Datadog'` | **drift**: matches no test, so it passes vacuously. Now `-run 'Datadog\|Bucket'`: 3 pass |
| 1 | `go test ./pkg/feeder/ -run 'Vocab'` | 5 pass |
| 1 | `go test ./internal/resolution/ -run 'C9'` | 5 pass |
| 1 | `go test ./internal/investigation/backend/ -run 'Unstamped'` | 2 pass |
| 2 | `go test ./pkg/feeder/versionstamp/...` | ok |
| 2 | `fixture verify fixtures/datadog-errors-by-version-01` | passed |
| 2 | `fixture verify fixtures/datadog-unstamped-01` | **drift**: no such fixture. T050 put the unstamped shape in `datadog-log-source-01` (`search`) with `TestTheUnstampedPointerIsAnsweredByTheEngine`. The quickstart now names both: passed, and 2 pass |
| 3 | `fixture verify fixtures/datadog-backend-logs-01` | passed |
| 4 | `fixture verify` monitor-transitions, grouped-monitor, doorbell-forged | 3 passed |
| 5 | `fixture verify` log-service-merge, log-rollout, log-rollout-merge | 3 passed |
| 5 | **drift**: the tags, SC-016 and budget fixtures came after the first draft, and the quickstart did not name them. Added: `datadog-tags-01`, `datadog-monitor-to-owner-01` with its CLI test, and §6b `datadog-rate-limited-01` with its test | all passed |
| 5 | `fixture verify --report --report-json report.jsonl fixtures/*/`; `check-report.sh` | 79 fixtures, every assertion held; `auto_merge/C8` and `auto_merge/C9` both present, with Datadog pairs |
| 6 | `go test … -run 'ReadOnly\|Published'` | **drift**: matched one test in two packages. Now `-run 'Published\|Surface\|NeverDeclared\|NoSelector\|Canary\|Clean'`: 7 pass |
| 6 | `./scripts/check-no-secrets.sh` | clean |

## Not run

§7 and §8 need a read-only Datadog key, and §8 also a named signatory (T099, T082, T083).

## Found on the way, and not changed here

- **An alert transition hides its monitor's definition properties.** From the transition onwards,
  the ALERT node's `service.name`, `deployment.environment.name` and `sre.datadog.monitor.type` read
  as absent. The transition is an assertion from the same source, and feature 001's fold rule
  (`internal/projector/segments.go`) takes one source's latest assertion whole. The answer to SC-016
  is unaffected: what the alert watches is its WATCHES edge. Changing the fold was a model decision for
  the owner; decided 2026-09-28, and the projector now folds a transition in its own lane beside the
  definition (`internal/projector/alert_transition.go`).
- `fixtures/datadog-log-service-merge-01` names a service `voice-agent`. It is a generic noun, not a
  vendor or an organisation's name, so it was left as it is.
