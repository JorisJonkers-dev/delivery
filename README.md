# delivery

The in-cluster services of the estate's delivery path
([JorisJonkers-dev/deploy-kit#195](https://github.com/JorisJonkers-dev/deploy-kit/issues/195)), in
Go, and the `delivery` project that deploys them. Generated from
[`template-go`](https://github.com/JorisJonkers-dev/template-go).

| Service | State |
|---------|-------|
| Release Gate: answers Flagger's webhooks and fails closed | probes only; its answers land with JorisJonkers-dev/delivery#2 |
| ClusterState Collector: commits the snapshot to the Estate repository | JorisJonkers-dev/delivery#4 |
| Vault policy job: applies the rendered policies and roles | JorisJonkers-dev/delivery#5 |

## What is in it

| Path | What it is |
|------|------------|
| `cmd/release-gate/` | The Release Gate binary: reads `ADDR` (default `:8080`), logs JSON with `slog`, drains on `SIGTERM` |
| `internal/server/` | The HTTP surface: `/healthz`, `/readyz`, graceful shutdown |
| `internal/deploykit/` | Go types generated from deploy-kit's published JSON Schemas, one package per schema, and the test that holds them to deploy-kit's corpus |
| `third_party/deploy-kit/` | The schemas, their accept/refuse corpus and the spec files the corpus reads, vendored at `DEPLOY_KIT_REF`; mirrors deploy-kit's `spec/v1/` |
| `scripts/sync-schemas.sh` | Fetches the vendored files from deploy-kit at one commit |
| `deploy/delivery.project.yml` | The `delivery` project's deploy-kit Project Intent: Flagger and the Release Gate |
| `Taskfile.yml` | `schemas:sync`, `schemas:check`, `gen`, `gen:check`, `lint`, `test`, `build`, `secrets`, and `check` (everything CI runs) |

The rest (`.golangci.yml`, `Dockerfile`, the workflows, release-please) is `template-go`'s,
unchanged but for the names; `mise.toml` adds `jq` for `schemas:sync`.

## Generated types

| Package | Root type | Schema |
|---------|-----------|--------|
| `internal/deploykit/resolved` | `ResolvedDeployment` | `resolved-deployment.schema.json` |
| `internal/deploykit/lock` | `CompositionLock` | `composition-lock.schema.json` |
| `internal/deploykit/pins` | `PinAnnotations` | `pin-annotations.schema.json` |
| `internal/deploykit/clusterstate` | `Snapshot` | `cluster-state-snapshot.schema.json` |

[go-jsonschema](https://github.com/atombender/go-jsonschema), pinned as a `tool` in `go.mod`,
generates each `types_gen.go`. Two gates hold the chain:

- `task schemas:check` fails when the vendored files differ from deploy-kit at `DEPLOY_KIT_REF`.
- `task gen:check` fails when a vendored schema changed and the types were not regenerated.

A generated type decodes every document the corpus accepts, and refuses on decode a missing
required field, a wrong type and a value outside an enum. It does not refuse an unknown field, a
union no branch of which matches, or a cross-field rule: a service that must refuse those
validates against the schema. The corpus test states this per schema.

To take a newer deploy-kit:

```bash
# move DEPLOY_KIT_REF in Taskfile.yml, then
task schemas:sync gen
task check
```

## Run it

```bash
mise install    # the pinned toolchain
task check      # lint, schemas:check, gen:check, tests with coverage, build, secret scan
docker build -t release-gate .
```

`task test` fails below 80% statement coverage of hand-written code (`COVERAGE_MIN` in
`Taskfile.yml`); generated files are left out of that figure.
