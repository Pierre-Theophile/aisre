#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fetch the upstream OpenTelemetry Demo Kubernetes manifest for the `full` overlay.
#
#   deploy/kind/otel-demo/fetch-upstream.sh            fetch if missing or stale
#   deploy/kind/otel-demo/fetch-upstream.sh --check     verify what is on disk, fetch nothing
#   deploy/kind/otel-demo/fetch-upstream.sh --force     re-download unconditionally
#
# The release is pinned and the download is checked against a recorded SHA-256, so the ~20k
# lines that land in full/upstream/ are reproducible without being committed. Override the
# release with OTEL_DEMO_VERSION=<tag>; the checksum is then only advisory and the script says
# so and prints the checksum it saw, so you can pin the new one.
#
# 2.2.0 is the last opentelemetry-demo release that ships kubernetes/opentelemetry-demo.yaml.
# 3.0.0 removed it: from 3.0.0 on, the Kubernetes deployment is the Helm chart only. Moving past
# 2.2.0 therefore means rendering the chart (`helm template`) into full/upstream/ yourself and
# regenerating full/collector-config.yaml from it — the recipe is in that file's header.

set -euo pipefail

PINNED_VERSION="2.2.0"
PINNED_SHA256="6d7e60bf1ba71a68d79ab3cac39482ab46627759ddcfcdb4e6a1d0645092ca3c"

VERSION="${OTEL_DEMO_VERSION:-$PINNED_VERSION}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DEST="$HERE/full/upstream/opentelemetry-demo.yaml"
URL="https://raw.githubusercontent.com/open-telemetry/opentelemetry-demo/${VERSION}/kubernetes/opentelemetry-demo.yaml"

MODE="fetch"
case "${1:-}" in
  --check) MODE="check" ;;
  --force) MODE="force" ;;
  "") ;;
  *) echo "usage: $(basename "$0") [--check|--force]" >&2; exit 2 ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

report() {
  local file="$1" actual
  actual="$(sha256_of "$file")"
  echo "  version  $VERSION"
  echo "  sha256   $actual"
  if [ "$VERSION" != "$PINNED_VERSION" ]; then
    echo "  note     OTEL_DEMO_VERSION overrides the pinned $PINNED_VERSION; checksum not enforced."
    echo "           full/collector-config.yaml was generated from $PINNED_VERSION and may no"
    echo "           longer match this release's collector configuration."
    return 0
  fi
  [ "$actual" = "$PINNED_SHA256" ]
}

if [ "$MODE" = "check" ]; then
  if [ ! -f "$DEST" ]; then
    echo "MISSING: $DEST"
    echo "Run $(basename "$0") to fetch opentelemetry-demo $VERSION."
    exit 1
  fi
  echo "Checking the upstream OpenTelemetry Demo manifest."
  if report "$DEST"; then
    echo "  result   OK, matches the pinned checksum"
    exit 0
  fi
  echo "  result   MISMATCH, expected $PINNED_SHA256"
  echo "Run $(basename "$0") --force to replace it."
  exit 1
fi

if [ "$MODE" = "fetch" ] && [ -f "$DEST" ] && [ "$VERSION" = "$PINNED_VERSION" ] &&
   [ "$(sha256_of "$DEST")" = "$PINNED_SHA256" ]; then
  echo "Upstream OpenTelemetry Demo $VERSION is already in place and matches its checksum."
  echo "  file     $DEST"
  echo "Nothing to do. Use --force to re-download."
  exit 0
fi

echo "Fetching the OpenTelemetry Demo Kubernetes manifest."
echo "  release  $VERSION"
echo "  from     $URL"
echo "  into     $DEST"

mkdir -p "$(dirname "$DEST")"
TMP="$(mktemp "${TMPDIR:-/tmp}/opentelemetry-demo.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

if ! curl -fsSL --retry 3 --retry-delay 2 -o "$TMP" "$URL"; then
  echo "Download failed. If $VERSION is a release newer than 3.0.0 it no longer ships this" >&2
  echo "file; see the header of this script." >&2
  exit 1
fi

if ! report "$TMP"; then
  echo "  result   CHECKSUM MISMATCH, expected $PINNED_SHA256" >&2
  echo "Refusing to install it. Either the release was re-tagged or the download was tampered" >&2
  echo "with; investigate before overriding." >&2
  exit 1
fi

mv "$TMP" "$DEST"
trap - EXIT
echo "  result   OK ($(wc -l < "$DEST" | tr -d ' ') lines)"
echo
echo "Now:  kubectl apply -k $HERE/full --context \"${KUBE_CONTEXT:-kind-sre-agent-demo}\""
