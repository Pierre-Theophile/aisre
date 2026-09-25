#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
#
# Spec-artifact guard (constitution §Development Workflow, §Fixture-first).
#
# The plan and task artifacts under specs/ are the primary deliverable of a whole phase of this
# project's method, and until this guard existed **nothing in CI read a line of them**. A pull
# request adding 3,400 lines of plan and 540 lines of tasks passed seven of eight jobs without any
# of them opening the files: the Go gates are thorough and entirely beside the point for a change
# that touches no Go.
#
# That gap matters because the failures are silent rather than loud. A task list whose ids were
# renumbered leaves its own cross-references pointing at the wrong tasks; a moved heading leaves
# every link to it dead; a half-applied convention leaves half the file meaning something else.
# None of that breaks a build, so none of it gets caught — it gets discovered later by
# somebody following an instruction that no longer means what it says.
#
# What is checked, and why each rule is the one that holds:
#
#   1. Task shape. `- [ ] T### [P?] [US?] description`, the format /speckit-tasks publishes. A
#      line that looks like a task and is not one is either a typo or a task nobody will execute.
#
#   2. Ids complete and unique — exactly T001..TN, no gaps, no duplicates. This is the invariant
#      that survives renumbering, which is the operation that actually goes wrong. Note it is NOT
#      "ascending in document order": specs/002 orders its phases by track rather than by id
#      (T060 follows T106) with all 120 ids present, and that is a legitimate layout choice.
#
#   3. A story label must match its phase — but only where the phase names a story. `[US3]` under
#      a "User Story 1" heading is a task filed against the wrong increment, which defeats the
#      point of organising by story. It is NOT an error to carry a story label under a phase named
#      for a technical area: specs/002 groups by area ("Phase 4: The query algebra…") and labels
#      each task with the story it serves, so one phase serves several stories. That is a
#      legitimate layout and an earlier draft of this guard wrongly failed all 60 of them.
#
#   4. No dangling task cross-reference. Every `T###` mentioned in prose must exist as a task in
#      that file. This is the one that catches renumbering damage, and it is why rule 2 exists.
#
#   5. Every relative link resolves, and 6. every heading anchor resolves — across every markdown
#      file under specs/ and docs/, not only the task lists.
#
#   7. **Every task names the fixture it validates against**, as a `; fixture: <name|unit|n/a>` note.
#      This is constitution §Development Workflow verbatim — "tasks MUST reference the replay fixtures
#      they validate against. A task that cannot name its fixture is not ready" — and all three
#      features satisfy it (001: 93/93, 002: 120/120, 003: 181/181).
#
#      An earlier draft of this rule only required the note to be applied all-or-none per file, which
#      let specs/003 pass with zero. That was a rule shaped around the non-compliance it should have
#      caught, and a /speckit-analyze pass found it. Do not weaken it back.
#
#   8. At least one task list exists. A guard whose inputs have vanished must not pass by doing no
#      work — the same refusal `expandFixtureArgs` makes for a fixture run with nothing to verify.
#      Found by writing the test: the first draft was silently clean on an empty tree.
#
# What is deliberately NOT checked, so nobody adds it back without reading this:
#
#   "Every task names a file path." A good guideline, and /speckit-tasks' own format rule, but not
#   a repository invariant: about twenty tasks across the two merged features legitimately name a
#   command and an output instead of a file — run a benchmark, verify a success criterion, write a
#   definition-of-done record. Enforcing it would mean editing completed history in two merged
#   features to satisfy a rule invented after they shipped. It stays a review question.
#
# Anchors are slugged the way GitHub does it: lowercase, drop everything that is not a word
# character, space or hyphen, then spaces to hyphens — and consecutive spaces are NOT collapsed,
# so "fact — this" becomes "fact--this". Getting that wrong produces false failures on every
# heading containing a dash, which is most of them in this repository.
#
# Exit 0 clean, 1 with every failure named. Fast: no network, no database, no Go build.

# The tree to check defaults to this script's own repository, which is what CI wants. It is an
# optional argument so that `scripts/check-specs_test.sh` can point the guard at a planted tree —
# a guard that cannot be aimed at anything cannot be tested, which is how the first draft of this
# file shipped nine rules none of which had ever been observed to fire.
set -eu

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT"

python3 - <<'PYEOF'
import os, re, sys, glob, collections

fail = []


def add(where, msg):
    fail.append(f"{where}: {msg}")


TASK = re.compile(r'^- \[[ xX]\] (T\d{3})( \[P\])?( \[(US\d+)\])? (.+)$')
LOOKS_LIKE_TASK = re.compile(r'^- \[[ xX]\] ')


def slug(heading):
    s = heading.lstrip('#').strip().lower()
    s = re.sub(r'[^\w\s-]', '', s, flags=re.UNICODE)
    return s.replace(' ', '-')


# ---- per-tasks.md rules (1-4, 7) -------------------------------------------------------------
task_lists = sorted(glob.glob('specs/*/tasks.md'))
if not task_lists:                                                           # 8
    add('specs/', "no task list found (specs/*/tasks.md); a guard whose inputs have "
                  "vanished must not pass by doing no work")

for path in task_lists:
    text = open(path, encoding='utf-8').read()
    lines = text.split('\n')
    phase, rows = None, []
    for i, line in enumerate(lines, 1):
        if line.startswith('## '):
            phase = line[3:].strip()
        if not LOOKS_LIKE_TASK.match(line):
            continue
        m = TASK.match(line)
        if not m:                                                            # 1
            add(path, f"line {i}: not the published task format "
                      f"`- [ ] T### [P?] [US?] description` -- {line[:70]}")
            continue
        rows.append((i, m.group(1), phase, m.group(4), m.group(5)))

    if not rows:
        add(path, "holds no tasks")
        continue

    nums = [int(t[1:]) for _, t, _, _, _ in rows]
    dupes = [f"T{n:03d}" for n, c in collections.Counter(nums).items() if c > 1]
    gaps = [f"T{n:03d}" for n in range(1, max(nums) + 1) if n not in set(nums)]
    if dupes:                                                                # 2
        add(path, f"duplicate task ids: {', '.join(sorted(dupes))}")
    if gaps:
        add(path, f"missing task ids: {', '.join(gaps)}")

    for ln, tid, ph, story, desc in rows:
        named = re.search(r'User Story (\d+)', ph or '')                     # 3
        if named and story and story != 'US' + named.group(1):
            add(path, f"line {ln}: {tid} labelled {story} under a "
                      f"User Story {named.group(1)} phase")
        if named and not story:
            add(path, f"line {ln}: {tid} is under '{ph}' but carries no [US#] label")

    known = {t for _, t, _, _, _ in rows}
    for ref in sorted({m.group(0) for m in re.finditer(r'T\d{3}', text)} - known):
        add(path, f"references {ref}, which is not a task in this file")     # 4

    without = [t for _, t, _, _, d in rows if 'fixture:' not in d]            # 7
    if without:
        add(path, f"{len(without)} of {len(rows)} tasks carry no '; fixture:' note "
                  f"(first: {', '.join(without[:5])}); constitution §Development Workflow: "
                  f"\"tasks MUST reference the replay fixtures they validate against\"")

# ---- repo-wide link and anchor rules (5, 6) ---------------------------------------------------
docs = sorted(glob.glob('specs/**/*.md', recursive=True) + glob.glob('docs/**/*.md', recursive=True))
anchors = {d: {slug(l) for l in open(d, encoding='utf-8') if l.startswith('#')} for d in docs}

for path in docs:
    base = os.path.dirname(path)
    for m in re.finditer(r'(?<!!)\[[^\]]*\]\((\.[^)\s]*?)(?:#([^)\s]+))?\)', open(path, encoding='utf-8').read()):
        target, anchor = m.group(1), m.group(2)
        resolved = os.path.normpath(os.path.join(base, target)) if target else path
        if not os.path.exists(resolved):                                     # 5
            add(path, f"link target does not exist: {target}")
            continue
        if anchor and resolved in anchors and anchor not in anchors[resolved]:  # 6
            add(path, f"link to {target}#{anchor}: no such heading in {resolved}")

if fail:
    print(f"check-specs: {len(fail)} problem(s)", file=sys.stderr)
    for f in fail:
        print(f"  {f}", file=sys.stderr)
    sys.exit(1)

n_tasks = sum(len(re.findall(r'^- \[[ xX]\] T\d{3}', open(p, encoding='utf-8').read(), re.M))
              for p in glob.glob('specs/*/tasks.md'))
print(f"check-specs: ok -- {n_tasks} tasks across "
      f"{len(task_lists)} task lists, {len(docs)} markdown files linked")
PYEOF
