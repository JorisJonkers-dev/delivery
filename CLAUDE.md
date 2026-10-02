# Agent contract

The estate-wide conventions live in one place and are **not duplicated here**:

**https://github.com/JorisJonkers-dev/workspace/blob/main/CLAUDE.md**

Read it before doing anything non-trivial in this repository. It covers the
things that most often go wrong, including:

- **Pull request labels.** The estate uses a prefixed taxonomy - `type:`,
  `area:`, `component:`, `priority:`, `status:`. Plain `bug` / `enhancement` /
  `documentation` do **not** exist, and `gh pr create` fails with
  `'bug' not found`. Run `gh label list --repo <owner>/<repo>` once before
  passing `--label`.
- **Verify the value, not the command.** An exit code, a `Ready` condition or
  an accepted object is not evidence that a consumer sees what you intended.
- Traps around workflow runs, `zsh` word-splitting, and detached submodule
  HEADs.

Duplicating that content into every repository guarantees the copies drift, so
this file stays a pointer. Add repo-specific guidance below.

## This repository

The estate's in-cluster delivery services, in Go: the Release Gate today, and the ClusterState
Collector and the Vault policy job as their tickets land (JorisJonkers-dev/deploy-kit#195).
`task check` is everything CI runs; `task` lists the targets. The toolchain is pinned in
`mise.toml` (`mise install`).

- `internal/deploykit/*/types_gen.go` is generated from the deploy-kit schemas vendored under
  `third_party/deploy-kit`. Never edit either by hand: move `DEPLOY_KIT_REF` in `Taskfile.yml`,
  then `task schemas:sync gen`.
- A type that refuses less than its schema is expected (an unknown field, a union, a
  cross-field rule); `internal/deploykit/corpus_test.go` states which breaks each type refuses.
