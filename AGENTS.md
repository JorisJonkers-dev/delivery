# Agents

Guidance for any coding agent working in this repository. It holds the Go in-cluster services of the estate's delivery path and the delivery project that deploys them.

The repo's agent contract is [CLAUDE.md](CLAUDE.md); the estate-wide conventions are at https://github.com/JorisJonkers-dev/workspace/blob/main/CLAUDE.md.

## Build and test

Tasks are defined in `Taskfile.yml`; `task --list` shows them. Tool versions are pinned in `mise.toml`.

- `task check` runs lint, schema and generation checks, tests, build and the secrets scan
- `task test` runs the tests alone
- `task gen` regenerates the generated code
