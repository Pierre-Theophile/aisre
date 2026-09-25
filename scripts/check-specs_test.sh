#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The spec guard's own test.
#
# A guard nobody has seen fail is a guard nobody knows works — the standard
# `scripts/check-no-secrets_test.sh` set for this repository, and the reason this file exists. It
# plants one violation of each rule check-specs.sh enforces, runs the guard over the planted tree,
# and asserts the matching rule fired. Then it asserts the guard is CLEAN on a well-formed tree, so
# that a rule which fires on everything is caught here rather than by failing every pull request.
#
# It also plants the shapes the guard must NOT reject, each taken from the real repository because
# an earlier draft rejected all of them:
#
#   - a story label under a phase named for a technical area rather than a story (specs/002's
#     layout, 60 tasks);
#   - task ids complete but not ascending in document order (specs/002 again, T060 after T106);
#   - a task naming a command and an output rather than a file (about twenty across 001 and 002).
#
# Writing this test is what found two defects in the guard itself: it could not be aimed at a tree
# other than its own repository, so none of its rules had ever been observed to fire; and it passed
# a tree containing no task lists at all, which is the vacuous pass this repository refuses
# elsewhere (`expandFixtureArgs` rejects a fixture run that would pass by doing no work).
#
# The planted tree is a temporary directory, so nothing is ever written into the repository.
#
#   scripts/check-specs_test.sh
#
# Exit 0 if every rule fired on its planted violation, no rule fired on a valid tree, and no
# legitimate shape was rejected. 1 otherwise, naming what went wrong.

set -uo pipefail

cd "$(dirname "$0")/.."
GUARD="$PWD/scripts/check-specs.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

TASKS="$work/specs/900-probe/tasks.md"
PLAN="$work/specs/900-probe/plan.md"
fails=0

# run <case> <expect: pass|fail> [grep-pattern-when-failing]
run() {
  local name="$1" expect="$2" pattern="${3:-}" out rc
  out="$("$GUARD" "$work" 2>&1)"; rc=$?
  if [ "$expect" = pass ]; then
    if [ $rc -ne 0 ]; then
      printf 'FAIL  %s: guard rejected a valid tree\n%s\n' "$name" "$out"; fails=$((fails + 1))
    else
      printf 'ok    %s (accepted)\n' "$name"
    fi
    return
  fi
  if [ $rc -eq 0 ]; then
    printf 'FAIL  %s: guard did NOT fire\n' "$name"; fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q -- "$pattern"; then
    printf 'FAIL  %s: fired, but not for the planted reason (wanted /%s/)\n%s\n' \
      "$name" "$pattern" "$out"; fails=$((fails + 1))
  else
    printf 'ok    %s (caught)\n' "$name"
  fi
}

# A minimal well-formed tree. Every case below starts from this and breaks exactly one thing.
scaffold() {
  rm -rf "$work/specs" "$work/docs"
  mkdir -p "$work/specs/900-probe" "$work/docs"
  cat > "$PLAN" <<'EOF'
# Plan

## A Heading — With A Dash

See [tasks](./tasks.md) and [the section](./plan.md#a-heading--with-a-dash).
EOF
  cat > "$TASKS" <<'EOF'
# Tasks

## Phase 1: Setup

- [ ] T001 Do a thing in `internal/a/a.go`; fixture: unit
- [ ] T002 [P] Re-run the coverage audit and publish the result; fixture: n/a

## Phase 2: User Story 1 - Something (Priority: P1)

- [ ] T003 [US1] Implement in `internal/c/c.go`, after T001; fixture: probe-01

## Phase 3: The algebra and the workers

- [ ] T004 [US2] A task under a technical phase, serving story 2, in `internal/d/d.go`; fixture: probe-02
EOF
}

scaffold
run "a valid tree" pass

scaffold
echo '- [ ] Do a thing with no id in `internal/e/e.go`; fixture: unit' >> "$TASKS"
run "rule 1 malformed task line" fail "not the published task format"

scaffold
echo '- [ ] T004 [US2] A duplicate id in `internal/f/f.go`; fixture: unit' >> "$TASKS"
run "rule 2 duplicate id" fail "duplicate task ids: T004"

# The renumbering failure this rule exists for.
scaffold
sed -i 's/^- \[ \] T002 /- [ ] T009 /' "$TASKS"
run "rule 2 missing id" fail "missing task ids"

scaffold
sed -i 's/^- \[ \] T003 \[US1\]/- [ ] T003 [US7]/' "$TASKS"
run "rule 3 label contradicts its phase" fail "labelled US7 under a User Story 1 phase"

scaffold
sed -i 's/^- \[ \] T003 \[US1\] /- [ ] T003 /' "$TASKS"
run "rule 3 missing label under a story phase" fail "carries no"

# What renumbering leaves behind.
scaffold
sed -i 's/after T001/after T055/' "$TASKS"
run "rule 4 dangling task reference" fail "references T055"

scaffold
sed -i 's|(\./tasks\.md)|(./nope.md)|' "$PLAN"
run "rule 5 dead relative link" fail "link target does not exist"

# The em-dash slug, broken in the other direction from the draft that got it wrong.
scaffold
sed -i 's|#a-heading--with-a-dash|#a-heading-with-a-dash|' "$PLAN"
run "rule 6 dead heading anchor" fail "no such heading"

scaffold
sed -i 's|`internal/a/a.go`; fixture: unit|`internal/a/a.go`|' "$TASKS"
run "rule 7 a task with no fixture note" fail "carry no '; fixture:' note"

scaffold
rm -f "$TASKS"
run "rule 8 no task list at all" fail "no task list"

# ---- shapes that must be accepted -------------------------------------------------------------

# All three published fixture vocabularies are accepted: a named fixture, `unit`, and `n/a`.
scaffold
sed -i 's|; fixture: probe-01|; fixture: unit|; s|; fixture: probe-02|; fixture: n/a|' "$TASKS"
run "rule 7 accepts a name, unit and n/a" pass

# specs/002's real layout: ids complete, document order not ascending.
scaffold
python3 - "$TASKS" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
a = '- [ ] T001 Do a thing in `internal/a/a.go`; fixture: unit\n'
b = '- [ ] T002 [P] Re-run the coverage audit and publish the result; fixture: n/a\n'
assert a in s and b in s, "scaffold changed; update this case"
open(p, 'w').write(s.replace(a + b, b + a))
PY
run "ids complete but not ascending (specs/002's layout)" pass

# The scaffold's T002 names a command and an output rather than a file, on purpose: about twenty
# tasks across the two merged features legitimately do, so the guard must not require a path.
scaffold
run "a task naming a command rather than a file" pass

# ---- and the guard must still be clean on the real repository ----------------------------------
# so that a rule which fires on everything is caught here rather than in CI.
if ! "$GUARD" > /dev/null 2>&1; then
  printf 'FAIL  guard is not clean on the real repository:\n'; "$GUARD" || true
  fails=$((fails + 1))
else
  printf 'ok    clean on the real repository\n'
fi

if [ "$fails" -ne 0 ]; then
  printf '\ncheck-specs_test: %d failure(s)\n' "$fails"; exit 1
fi
printf '\ncheck-specs_test: every rule fired on its planted violation; no false positives\n'
