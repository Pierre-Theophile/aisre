#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0
#
# Prepend the SPDX licence identifier to generated Go sources.
#
# protoc-gen-go and protoc-gen-connect-go do not emit licence headers, but every Go file in
# this repository must carry one (goheader rule in .golangci.yml, constitution IX). This runs
# after `buf generate` and is idempotent, so `make gen` produces a stable tree.

set -eu

HEADER='// SPDX-License-Identifier: Apache-2.0'
ROOT=$(cd "$(dirname "$0")/.." && pwd)

find "$ROOT/api" -name '*.go' -type f | while read -r file; do
	if [ "$(head -n 1 "$file")" = "$HEADER" ]; then
		continue
	fi
	tmp="$file.spdx.tmp"
	{
		printf '%s\n\n' "$HEADER"
		cat "$file"
	} >"$tmp"
	mv "$tmp" "$file"
	echo "spdx: $file"
done
