# delivery

The in-cluster services of the estate's delivery path
([JorisJonkers-dev/deploy-kit#195](https://github.com/JorisJonkers-dev/deploy-kit/issues/195)), in
Go, and the `delivery` project that deploys them. Generated from
[`template-go`](https://github.com/JorisJonkers-dev/template-go).

| Service | State |
|---------|-------|
| Release Gate: answers Flagger's webhooks and fails closed | probes only; its answers land with JorisJonkers-dev/delivery#2 |
| ClusterState Collector: commits the snapshot to the Estate repository | built and tested; not deployed yet, see [The Collector](#the-collector) |
| Vault policy job: applies the rendered policies and roles | JorisJonkers-dev/delivery#5 |

## What is in it

| Path | What it is |
|------|------------|
| `cmd/release-gate/` | The Release Gate binary: reads `ADDR` (default `:8080`), logs JSON with `slog`, drains on `SIGTERM` |
| `internal/server/` | The HTTP surface: `/healthz`, `/readyz`, graceful shutdown |
| `cmd/collector/` | The ClusterState Collector binary: runs once and exits |
| `internal/collector/` | What the Collector captures, the snapshot it writes, and the envtest case that holds its reads and its grant |
| `internal/githubapp/` | One file of one repository, read and written as a GitHub App installation |
| `deploy/collector/rbac.yaml` | Everything the Collector may do in the cluster: get and list three kinds |
| `internal/deploykit/` | Go types generated from deploy-kit's published JSON Schemas, one package per schema, and the test that holds them to deploy-kit's corpus |
| `third_party/deploy-kit/` | The schemas, their accept/refuse corpus and the spec files the corpus reads, vendored at `DEPLOY_KIT_REF`; mirrors deploy-kit's `spec/v1/` |
| `scripts/sync-schemas.sh` | Fetches the vendored files from deploy-kit at one commit |
| `deploy/delivery.project.yml` | The `delivery` project's deploy-kit Project Intent: Flagger and the Release Gate |
| `Taskfile.yml` | `schemas:sync`, `schemas:check`, `gen`, `gen:check`, `lint`, `test`, `build`, `secrets`, and `check` (everything CI runs) |

The rest (`.golangci.yml`, the workflows, release-please) is `template-go`'s, unchanged but for
the names; `mise.toml` adds `jq` for `schemas:sync`. The `Dockerfile` builds one command per image,
chosen with `--build-arg APP=`.

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

## The Collector

Each run lists PersistentVolumes, PersistentVolumeClaims and the pods deploy-kit renders, and
compares what they say with `cluster-state.yml` in the Estate repository
([deploy-kit `spec/v1/20-resolved-deployment.md`](https://github.com/JorisJonkers-dev/deploy-kit/blob/main/spec/v1/20-resolved-deployment.md#the-collector)):

| fact | read from |
|---|---|
| a binding, `{claim, node}` | a bound claim's name, and the one node its volume's node affinity names under `kubernetes.io/hostname`. A volume any node can mount binds its claim to nothing |
| a placement, `{process, node}` | a pod labelled `app.kubernetes.io/managed-by: deploy-kit`: its `app.kubernetes.io/name` and the node it is scheduled to. A pod that has finished is not a placement |

Facts are ordered and free of duplicates. When they equal the committed snapshot's, the run
commits nothing, and `capturedAt` stays what it was. When they differ, the run writes the file
once, through the Collector's GitHub App, replacing exactly the blob it read. A snapshot that
names another cluster, or that deploy-kit's schema would refuse, is never overwritten.

| variable | value |
|---|---|
| `CLUSTER_NAME` | the cluster, as the snapshot names it |
| `ESTATE_REPOSITORY` | `JorisJonkers-dev/estate` |
| `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID` | the Collector's App and its installation on the Estate repository |
| `GITHUB_APP_PRIVATE_KEY_FILE` | where the Secret Store projects the App's private key |

**Not deployed yet.** The spec makes the Collector a `CronJob` of this project, and Project Intent
has no schedule and no cluster-wide read to declare one with, so it is not in
`deploy/delivery.project.yml`. `deploy/collector/rbac.yaml` is the grant the envtest case proves;
the `CronJob` around it waits for that decision in deploy-kit.

## Run it

```bash
mise install    # the pinned toolchain
task check      # lint, schemas:check, gen:check, tests with coverage, build, secret scan
docker build --build-arg APP=release-gate -t release-gate .
docker build --build-arg APP=collector -t collector .
```

`task test` fails below 80% statement coverage of hand-written code (`COVERAGE_MIN` in
`Taskfile.yml`); generated files and the fake GitHub the tests run against are left out of that
figure. The Collector's envtest case starts a real API server, which `task test` fetches once
through `setup-envtest`.
