# Coverage-audit fixtures

Inputs for `sre-agent audit coverage` (spec 002 User Story 0, FR-069 to FR-071b). They are the
test instrument for the audit command, the comparison, the ceiling guard and the π₀ hook.

**Everything here is synthetic.** No real incident, and no reconstruction of one, is in this
directory or ever will be. The real audit's incident-level detail is private (FR-069a); what is
public is its aggregate, at [`docs/evaluation/coverage-audit-2026-09.md`](../../docs/evaluation/coverage-audit-2026-09.md).
These fixtures reproduce the *shape* of that aggregate — the counts, the ladder, the arithmetic —
so the tests can assert a real ceiling without any real data.

| fixture | what it is | ceiling | π₀ |
|---|---|---|---|
| `synthetic-01` | 13 invented incidents, three cumulative feeder sets, the published 0 / 3 / 8 verdict split | `8/13 = 0.615385` | `0.384615` |
| `synthetic-02` | the same 13 incidents re-audited after a connector shipped: a fourth feeder set, one incident moved | `9/13 = 0.692308` | `0.307692` |

`synthetic-02` exists for `audit coverage compare`: exactly one incident moves, exactly one
feeder set is added, and the three older rungs of the ladder did not move — which is the case
FR-071a cares most about, because "did not move" must be reported rather than omitted.

The input format is documented at
[`docs/evaluation/coverage-audit-format.md`](../../docs/evaluation/coverage-audit-format.md),
including how the owner transcribes a private audit into it without any of the private audit
leaking.
