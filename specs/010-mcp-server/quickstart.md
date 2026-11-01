# Quickstart: aisre over MCP

**Feature**: [spec.md](./spec.md)

## Local, over stdio (Claude Code)

```sh
bin/aisre serve --db "$PG_DSN" --listen 127.0.0.1:8080 --auth dev --dev &
bin/aisre fixture load --db "$PG_DSN" fixtures/rollout-regression-01
bin/aisre dev-token --dev --user alice --roles r,i > ~/.config/aisre/token
claude mcp add aisre -- aisre mcp --server http://127.0.0.1:8080 --token-file ~/.config/aisre/token
```

Then ask: "what changed around checkout between 13:00 and 14:32 on 2026-09-01?" The agent calls `diff`.

## Cursor, Codex and other clients

Any client that launches a stdio server: command `aisre`, arguments `mcp --server <url> --token-file <path>`.

## Shared, over streamable HTTP

```sh
aisre mcp --http 0.0.0.0:8090 --tls-cert cert.pem --tls-key key.pem --server https://aisre.internal
```

Each client sends its own `Authorization: Bearer <token>`; the server forwards it and holds none.

## Evaluation baseline

```sh
aisre mcp --profile baseline --server <url> --token-file <path>   # graph and catalog tools only
```

## Validation

1. `tools/list` with a reader token lists 12 tools; with an investigator token 13; under `--profile baseline` 10; with a feeder token
   the server refuses to start (stdio) or answers 403 (HTTP).
2. `diff` on `rollout-regression-01` equals `fixtures/rollout-regression-01/golden/diff.checkout-diff.json`.
3. `pointers` on `incidents/injection-01` carries a non-empty `aisre.injection`.
