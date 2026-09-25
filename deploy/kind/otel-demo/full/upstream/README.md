<!-- SPDX-License-Identifier: Apache-2.0 -->

# `upstream/` — fetched, not vendored

`opentelemetry-demo.yaml` belongs here and is not in git. Put it there with:

```sh
deploy/kind/otel-demo/fetch-upstream.sh
```

The script pins the release (`OTEL_DEMO_VERSION`, default `2.2.0`) and verifies the download
against a recorded SHA-256, so two people running it get the same bytes. `--check` verifies an
existing copy without downloading; `--force` re-downloads.

The file is ~20k lines of manifests generated from the `opentelemetry-demo` Helm chart. It is
kept out of the repository so that diffs stay readable; the only thing this project asserts
about it is the checksum and the shape of the collector ConfigMap that `../collector-config.yaml`
replaces.
