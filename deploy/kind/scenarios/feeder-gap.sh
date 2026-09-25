#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scenario: the Kubernetes feeder is down while the cluster changes underneath it.
#
#   deploy/kind/scenarios/feeder-gap.sh                      stop the feeder, delete
#                                                            svc/checkout, wait, restore, restart
#   deploy/kind/scenarios/feeder-gap.sh --gap 300 --service storefront
#   deploy/kind/scenarios/feeder-gap.sh --keep-feeder        do not touch any process; just
#                                                            delete, wait and restore
#
# This is the feeder-gap fixture (T067) and the "feeder gap" edge case: something is deleted
# while nobody is watching, and the graph has to end up with the retraction's valid_end inside
# the gap and its observed time at reconnection, with `extent` reporting the gap (FR-052).
#
# What it does to your feeder
# ---------------------------
# By default it looks for a running process whose command line matches FEEDER_PATTERN
# (default: "aisre feed k8s"), remembers that command line verbatim, sends it SIGTERM, and
# starts it again — same command line — after the gap, with its output going to a log file it
# names. If no such process is running it says so and carries on; if you would rather drive the
# feeder yourself, pass --keep-feeder and the script will tell you when to stop and start it.
#
# It never kills anything it did not match, and it never invents feeder arguments.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
NS="${SHOP_NAMESPACE:-shop}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OVERLAY="${OVERLAY_DIR:-$HERE/../otel-demo/minimal}"
FEEDER_PATTERN="${FEEDER_PATTERN:-sre-agent feed k8s}"
LOG_DIR="${FEEDER_LOG_DIR:-${TMPDIR:-/tmp}}"

GAP="${GAP_SECONDS:-120}"
# How long the reconnected feeder is given to list the namespace and emit its retraction before
# the Service is put back. Too short and the restore races the initial list, and the recording
# shows neither a clean retraction nor a clean restore.
RELIST="${RELIST_SECONDS:-30}"
SERVICE="checkout"
MANAGE_FEEDER=1

while [ $# -gt 0 ]; do
  case "$1" in
    --gap) GAP="${2:?--gap needs a value}"; shift 2 ;;
    --service) SERVICE="${2:?--service needs a value}"; shift 2 ;;
    --keep-feeder) MANAGE_FEEDER=0; shift ;;
    *) echo "usage: $(basename "$0") [--gap SECONDS] [--service NAME] [--keep-feeder]" >&2; exit 2 ;;
  esac
done

k() { kubectl --context "$CONTEXT" "$@"; }

k get service "$SERVICE" -n "$NS" >/dev/null

echo "Feeder gap scenario."
echo "  context   $CONTEXT"
echo "  gap       ${GAP}s"
echo "  deleting  service/$SERVICE in $NS, restored from $OVERLAY ${RELIST}s after the"
echo "            feeder reconnects — not before, or there is nothing to retract"
echo

FEEDER_CMD=""
if [ "$MANAGE_FEEDER" = "1" ]; then
  # -f matches the whole command line; take the first match only.
  FEEDER_PID="$(pgrep -f "$FEEDER_PATTERN" | head -n1 || true)"
  if [ -n "$FEEDER_PID" ]; then
    FEEDER_CMD="$(ps -o command= -p "$FEEDER_PID" | sed 's/^ *//')"
    echo "Stopping the feeder."
    echo "  pid       $FEEDER_PID"
    echo "  command   $FEEDER_CMD"
    kill -TERM "$FEEDER_PID"
    for _ in $(seq 1 30); do
      kill -0 "$FEEDER_PID" 2>/dev/null || break
      sleep 1
    done
    if kill -0 "$FEEDER_PID" 2>/dev/null; then
      echo "  warning   pid $FEEDER_PID is still alive after 30s; carrying on anyway"
    else
      echo "  stopped"
    fi
  else
    echo "No process matching \"$FEEDER_PATTERN\" is running; nothing to stop."
    echo "The gap will still be real if your feeder is down for other reasons."
  fi
else
  echo "--keep-feeder: stop your Kubernetes feeder NOW, then press Enter."
  # `|| true`: under `set -e` a read that hits EOF — no terminal, which is every CI job and
  # every `< /dev/null` — would end the script here, silently, before the Service is deleted.
  # The prompt is a courtesy to a human; it must not be a precondition.
  read -r _ || true
fi

GAP_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
echo "Deleting service/$SERVICE in $NS at $GAP_START."
k -n "$NS" delete service "$SERVICE" --ignore-not-found
echo "Deleted. Waiting ${GAP}s with nobody watching."
sleep "$GAP"

# The feeder comes back BEFORE the Service does, and that order is the whole scenario.
#
# A reconnecting feeder has no memory: it lists the namespace and reports what is there. If the
# Service were restored first it would be there, the feeder would see nothing missing, and the
# run would produce no retraction at all — only a new resourceVersion for an object that never
# appeared to leave. Reconnecting into the absence is what makes the graph say "this exposure
# ended, some time inside the window nobody was watching", which is the fact FR-052 and the
# feeder-gap fixture are about.
GAP_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo
if [ "$MANAGE_FEEDER" = "1" ] && [ -n "$FEEDER_CMD" ]; then
  LOG="$LOG_DIR/sre-agent-feed-k8s-$(date -u +%Y%m%dT%H%M%SZ).log"
  echo "Restarting the feeder with the same command line, while $SERVICE is still absent."
  echo "  command   $FEEDER_CMD"
  echo "  log       $LOG"
  case "$FEEDER_CMD" in
    *--record*)
      echo "  WARNING   that command line records into a directory that already holds this run's"
      echo "            payloads. events.jsonl is appended to, but payload files are numbered"
      echo "            from 000001 again and overwrite the earlier ones while the index still"
      echo "            points at them. For a recording you intend to keep, use --keep-feeder"
      echo "            and restart the feeder yourself with a second --record directory."
      ;;
  esac
  # shellcheck disable=SC2086
  nohup sh -c "$FEEDER_CMD" >"$LOG" 2>&1 &
  echo "  pid       $!"
elif [ "$MANAGE_FEEDER" = "0" ]; then
  echo "--keep-feeder: start your Kubernetes feeder again NOW, while service/$SERVICE is still"
  echo "absent, and press Enter once it has logged its initial list."
  read -r _ || true
fi

echo
echo "Giving the feeder ${RELIST}s to finish its initial list and retract the exposure."
sleep "$RELIST"

echo
echo "Restoring service/$SERVICE."
if [ -f "$OVERLAY/$SERVICE.yaml" ]; then
  k apply -f "$OVERLAY/$SERVICE.yaml"
else
  echo "  no $OVERLAY/$SERVICE.yaml; reapplying the whole overlay instead"
  k apply -k "$OVERLAY"
fi
RESTORED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo
echo "Gap ran from $GAP_START to $GAP_END; service/$SERVICE restored at $RESTORED_AT."
echo "Expect: service/$SERVICE retracted with valid_end inside the gap and observed at"
echo "reconnection, and the gap itself visible in:"
echo "  aisre extent"
