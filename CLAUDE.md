# Guidance for agents working in this repository

This file is the runtime guidance the constitution
([`.specify/memory/constitution.md`](.specify/memory/constitution.md)) points to. Where the two
disagree, the constitution wins.

## Merging pull requests

- **CI green means merge it yourself.** When every check on a pull request's latest commit has
  passed and GitHub reports no merge conflict, the agent that drives the pull request merges it,
  with a merge commit, without waiting to be asked. This is the project owner's standing rule
  (2026-10-14).
- A red or conflicted pull request is never merged: fix it first, and merge once it is green.
