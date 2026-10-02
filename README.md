# template-go

A GitHub template repository for a Go service or controller in the
[JorisJonkers-dev](https://github.com/JorisJonkers-dev) estate. A repository generated from it builds,
tests and lints green with no edits.

The estate's decisions on this shape (the Go and Vue standard, and the repository templates) live in
[`JorisJonkers-dev/workspace` docs/decisions](https://github.com/JorisJonkers-dev/workspace/tree/main/docs/decisions).
Hygiene files (licence, security policy, `CODEOWNERS`, Renovate, editor config, release flow) come
from [`repo-template`](https://github.com/JorisJonkers-dev/repo-template).

## What is in it

| Path | What it is |
|------|------------|
| `cmd/template-go/` | The binary: reads `ADDR` (default `:8080`), logs JSON with `slog`, drains on `SIGTERM` |
| `internal/server/` | The HTTP surface: `/healthz`, `/readyz`, graceful shutdown; unit tests |
| `mise.toml` | Pins Go, task, golangci-lint, actionlint and gitleaks; `mise install` is the only setup |
| `Taskfile.yml` | `gen`, `gen:check`, `lint`, `test`, `build`, `secrets`, and `check` (everything CI runs) |
| `.golangci.yml` | golangci-lint v2 with gofumpt and goimports; zero issues required |
| `Dockerfile` | Multi-stage, static binary on `distroless/static:nonroot` |
| `.github/workflows/ci.yml` | One job, `Pipeline Complete`: `mise exec -- task check`, then `docker build` |
| `.github/workflows/release.yml`, `release-please-config.json`, `.release-please-manifest.json` | release-please, as in the rest of the estate |
| `deploy/template-go.project.yml` | A [deploy-kit](https://github.com/JorisJonkers-dev/deploy-kit) Project Intent for one stateless HTTP service |

`task test` runs the race detector and fails below 80% statement coverage (`COVERAGE_MIN` in
`Taskfile.yml`); the sample code sits at about 87%. `task gen` runs `go generate ./...` and
`task gen:check` fails on a dirty or untracked result. Nothing generates yet, so both are no-ops.

[`go-commons`](https://github.com/JorisJonkers-dev/go-commons) is used where it applies. It has no
packages yet, so the template does not depend on it.

## Use it

1. **Create the repository** with "Use this template" on GitHub.
2. **Rename the module.** Replace `github.com/JorisJonkers-dev/template-go` with the new module path in
   `go.mod`, `.golangci.yml` (the goimports prefix) and every import.
3. **Rename the service.** Rename `cmd/template-go/` to `cmd/<name>`, then replace `template-go` in
   `Taskfile.yml` (`APP`), `Dockerfile`, `.github/workflows/ci.yml` and `release.yml`.
4. **Rename the project file.** Move `deploy/template-go.project.yml` to `deploy/<name>.project.yml`
   and change `project`, `owner`, the Application `id`, the Process `name` and `image`, and the host.
5. **Reset release state**: delete `CHANGELOG.md` if present and keep `.release-please-manifest.json`
   at `0.0.0`.
6. `mise install && task check`.

```bash
mise install    # the pinned toolchain
task check      # lint, gen:check, tests with coverage, build, secret scan
docker build -t template-go .
```

Ruleset and project boarding are described in [`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`VERSIONING.md`](VERSIONING.md); `add-to-project.yml` should be deleted in a private repository,
as its header explains.
