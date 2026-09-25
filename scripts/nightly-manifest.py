#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Finish the manifest `aisre fixture assemble` wrote, for the nightly live run (T082).

    scripts/nightly-manifest.py <assembled-manifest> [<model-manifest>]

`fixture assemble` writes the half of a manifest a machine knows: the sources, the files, the
clock. What it deliberately leaves out is the half that makes a fixture worth having - the
queries whose answers become the goldens, and the ground truth they are measured against -
because on a fixture somebody is about to commit, those are a human's decision.

The nightly run is the one case where they can be derived, because the run is scripted: the
shop is always the `minimal` overlay, the change is always `scenarios/rollout.sh` rolling
`shop/payments`, and the instants that matter are the recording's own clock. So this writes:

  * a `diff` over the whole run focused on `otel.service.name=checkout`, which is the query the
    rollout-regression family exists for;
  * a `subgraph` at the end of the run, which is what the SC-010 live-versus-replayed comparison
    is made over;
  * `ground_truth.culprit_change`, read out of the recorded events - the Kubernetes rollout
    change node targeting `shop/payments`. If the run did not record one, the key is left out
    and the report simply has no ranking to measure, rather than a ranking measured against a
    guess.

<model-manifest> is optional and is used only for its `expected_top_k`, so that the nightly is
held to the same bar as the committed fixture of the same family.

It is a script rather than a flag on `fixture assemble` because it encodes what *this scenario*
does, not what assembling means.
"""
import json
import os
import sys

MODEL_DEFAULT_TOP_K = 3


def load_clock(manifest_text):
    """The clock block assemble wrote, as (start, end) RFC3339 strings."""
    start = end = None
    in_clock = False
    for line in manifest_text.splitlines():
        if line.startswith("clock:"):
            in_clock = True
            continue
        if in_clock:
            if line[:1] not in (" ", "\t"):
                break
            key, _, value = line.strip().partition(":")
            value = value.strip()
            if key == "start":
                start = value
            elif key == "end":
                end = value
    if not start or not end:
        sys.exit("nightly-manifest: the assembled manifest has no clock; "
                 "fixture assemble derives it from payloads/index.jsonl")
    return start, end


def find_culprit(fixture_dir):
    """The k8s rollout change node targeting shop/payments, as a manifest culprit ref."""
    events = os.path.join(fixture_dir, "events.jsonl")
    if not os.path.exists(events):
        return None
    culprit = None
    with open(events, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            event = json.loads(line)
            change = event.get("observeChange")
            if not change:
                continue
            ref = change.get("ref", {})
            if ref.get("namespace") != "k8s.change":
                continue
            if change.get("change", {}).get("kind") != "ROLLOUT":
                continue
            targets = change.get("targets", [])
            if not any(t.get("value", "").endswith("/payments") for t in targets):
                continue
            # The last one wins: a run that rolls twice is measured against the change the
            # scenario made, not against the initial deploy it inherited.
            culprit = "%s=%s" % (ref["namespace"], ref["value"])
    return culprit


def expected_top_k(model):
    if not model or not os.path.exists(model):
        return MODEL_DEFAULT_TOP_K
    with open(model, encoding="utf-8") as handle:
        for line in handle:
            key, _, value = line.strip().partition(":")
            if key == "expected_top_k" and value.strip().isdigit():
                return int(value.strip())
    return MODEL_DEFAULT_TOP_K


def main(manifest_path, model_path):
    with open(manifest_path, encoding="utf-8") as handle:
        text = handle.read()
    if "\nqueries:" in text:
        sys.exit("nightly-manifest: %s already has a queries block" % manifest_path)

    fixture_dir = os.path.dirname(os.path.abspath(manifest_path))
    start, end = load_clock(text)
    culprit = find_culprit(fixture_dir)

    block = [
        "",
        "# Written by scripts/nightly-manifest.py: the queries this scripted run can name for",
        "# itself. The instants are the recording's own clock.",
        "queries:",
        "  - name: checkout-diff",
        "    kind: diff",
        "    focus: otel.service.name=checkout",
        "    t1: %s" % start,
        "    t2: %s" % end,
        "    hops: 2",
        "    direction: both",
        "  - name: checkout-2hop",
        "    kind: subgraph",
        "    focus: otel.service.name=checkout",
        "    valid_at: %s" % end,
        "    hops: 2",
        "    direction: both",
        "",
    ]
    if culprit:
        block += [
            "ground_truth:",
            "  culprit_change: %s" % culprit,
            "  expected_top_k: %d" % expected_top_k(model_path),
            "",
        ]
    else:
        block += [
            "# No Kubernetes rollout change node targeting shop/payments was recorded, so this",
            "# run has no culprit to measure a ranking against. That is a finding about the run,",
            "# not a reason to invent one.",
            "",
        ]

    with open(manifest_path, "w", encoding="utf-8") as handle:
        handle.write(text.rstrip("\n") + "\n" + "\n".join(block))

    print("nightly-manifest: clock %s .. %s" % (start, end))
    print("nightly-manifest: culprit %s" % (culprit or "(none recorded)"))


if __name__ == "__main__":
    if len(sys.argv) not in (2, 3):
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2] if len(sys.argv) == 3 else None)
