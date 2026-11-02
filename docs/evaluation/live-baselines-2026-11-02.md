<!-- SPDX-License-Identifier: Apache-2.0 -->

# Live run with the baselines, 2026-11-02

The first run of the public corpus with a real model since the simple harness (002 T131) landed.
It was run to get the numbers T132 needs before an expectation table may move to `enforce`.

```sh
aisre eval run --fixtures fixtures/incidents --runs 1 --extend-to 1 --live
```

Model configuration 1.2.0 (digest `e706775f`): the investigator is `zai-glm-5-3` on Mistral. The
simple harness uses the same investigator model. One run per fixture, so these are pass@1 numbers on
20 fixtures, not a gated result: the gate needs `--runs 3`.

## Verdicts, all 20 fixtures

Each answer is scored by the same verdict-only rule (`VerdictRight`, FR-059b).

| answerer | right | share |
|---|---:|---:|
| the engine | 15 of 20 | 75 % |
| the simple harness (one general agent, MCP `baseline` profile, no engine) | 12 of 20 | 60 % |
| the telemetry-free floor (prior-only ranking) | 9 of 20 | 45 % |

Where they differ:

- The simple harness names a change on all four `unobserved-*` fixtures, whose truth is that no
  observed change caused the symptom. The engine is right on all four. Without telemetry,
  the agent has no way to rule a change out, so it picks the closest one.
- The simple harness is right where the engine is wrong on `adjacent-decoy-01` and
  `stopped-flow-01`. Both are verdicts the engine still gets wrong
  (see the edge F1 note in 002 T134).
- Both are wrong on `telemetry-rejection-01`, `two-simultaneous-01` and `unknown-feeder-gap-01`
  (the simple harness is also wrong on `distant-culprit-01`).

The corpus aggregates in the report rows cover only the 4 fixtures admitted on their miss rate
(engine 2 of 4, simple harness 3 of 4, floor 2 of 4). A live investigator asks terms the recorded
worlds were not recorded for, so 16 of 20 fixtures are above their admission bar on this run. That
is a recording gap, not a scoring result.

## The expectation table (T132)

Omission rate 0: no supported ground-truth hypothesis left a decisive family reachable and
unqueried, on any fixture where it was measured. The same is true on the model-free run. An
enforced v1 would therefore have held nothing back and corrected nothing. It cannot pass ADR-0019
D4's dual gate, whose first half asks for strictly better results on the cases. **v1 stays in
`observe`.** The hold itself is built and tested, so the move is a one-line change once a corpus
run shows omissions that enforcing would have corrected.

## Also seen

- Citation validity was 82 of 86 on the admitted fixtures. The first invalid citation is a
  `knowledge_search` answer with outcome `no_data` that the recording does not hold. This is a gated
  metric and it needs its own look.
- Cost: $0.07 for the engine runs on the admitted fixtures.
