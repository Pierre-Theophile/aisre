#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# netns.sh — run a command inside a network namespace that holds nothing but a down loopback.
#
# This is how the trajectory-replay gate makes "zero network" a property of the environment
# rather than an assertion in the code (spec 002 FR-039–FR-041, SC-003; constitution §Replay CI).
# Inside the namespace there is no route, no resolver and no socket to anywhere: a call to a
# vendor, a telemetry backend or a database fails at once with "network is unreachable".
#
# Three ways in, tried in order, because the runner images differ in what they allow:
#
#   1. already root            → `unshare --net`, nothing else needed;
#   2. unprivileged user ns    → `unshare --map-root-user --net`, the classic `unshare -rn`;
#   3. neither (Ubuntu 24.04 images restrict unprivileged user namespaces through AppArmor, so
#      writing /proc/self/uid_map is "Operation not permitted") → root creates the namespace
#      with passwordless sudo, then `setpriv` drops straight back to the invoking user and
#      group before the command runs, so files it writes are owned by the caller, not root.
#
# The command runs with the caller's uid/gid and working directory in every case. Stdout and
# stderr are the command's own, so `netns.sh cmd | tee` behaves exactly like `cmd | tee`.
#
# It is Linux-only: macOS has no network namespaces. The local equivalent on a Mac is to trust
# `fixture verify --trajectory-only`'s own report, which names every worker call it served from
# the recording; the CI gate is where the property is enforced.
set -euo pipefail

if [ $# -eq 0 ]; then
	echo "usage: scripts/netns.sh <command> [args...]" >&2
	exit 2
fi

if ! command -v unshare > /dev/null 2>&1; then
	echo "netns.sh: unshare(1) not found; this script needs Linux util-linux" >&2
	exit 2
fi

# 1. root already.
if [ "$(id -u)" -eq 0 ]; then
	exec unshare --net -- "$@"
fi

# 2. unprivileged user namespaces allowed.
if unshare --map-root-user --net -- true 2> /dev/null; then
	exec unshare --map-root-user --net -- "$@"
fi

# 3. root creates it, setpriv hands it back to us.
if ! command -v setpriv > /dev/null 2>&1; then
	echo "netns.sh: unprivileged user namespaces are disabled and setpriv(1) is missing" >&2
	exit 2
fi
if ! sudo -n true 2> /dev/null; then
	echo "netns.sh: unprivileged user namespaces are disabled and passwordless sudo is unavailable" >&2
	exit 2
fi
exec sudo -n unshare --net -- \
	setpriv --reuid="$(id -u)" --regid="$(id -g)" --clear-groups -- "$@"
