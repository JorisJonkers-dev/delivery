# delivery

The in-cluster services of the estate's delivery path
([JorisJonkers-dev/deploy-kit#195](https://github.com/JorisJonkers-dev/deploy-kit/issues/195)), in
Go, and the `delivery` project that deploys them. Generated from
[`template-go`](https://github.com/JorisJonkers-dev/template-go).

| Service | State |
|---------|-------|
| Release Gate: answers Flagger's webhooks and fails closed | answers all three, starts and undoes migrations, and logs held releases, see [The Release Gate](#the-release-gate); not deployed yet |
| ClusterState Collector: commits the snapshot to the Estate repository | built and tested; not deployed yet, see [The Collector](#the-collector) |
| Reporter: keeps every Rollout's GitHub Deployment current, stage by stage | built and tested; not deployed yet, see [The Reporter](#the-reporter). Writes no warmup stage until the Release Gate warms up |
| Vault policy job: applies the rendered policies and roles | built and tested against a dev Vault, see [The Vault policy job](#the-vault-policy-job); not deployed yet |
| Catalog method: `postgres-catalog`, the image the Platform document's `postgres` engine names to apply its database catalog | built and tested against a real Postgres and a dev Vault, see [The catalog method](#the-catalog-method); not deployed yet |
| Backup methods: `file-backup`, `postgres-backup`, `rabbitmq-backup`, the images the Platform document's engines name | built and tested; each writes one generation into the backup claim and keeps the newest `BACKUP_RETAIN`. The two network methods dump the peer the render hands them as `BACKUP_HOST` and `BACKUP_PORT`, logging in with the credential's keys (`PGUSER` and `PGPASSWORD`, `RABBITMQ_USERNAME` and `RABBITMQ_PASSWORD`) ([deploy-kit#284](https://github.com/JorisJonkers-dev/deploy-kit/issues/284)). A Rollback's one-shot run, handed `BACKUP_ROLLBACK`, writes `rollback-<request>-<generation>`, which `BACKUP_RETAIN` never counts and every run deletes after 7 days. No off-cluster copy yet |

## What is in it

| Path | What it is |
|------|------------|
| `cmd/release-gate/` | The Release Gate binary: reads `ADDR` (default `:8080`) and the cluster it runs in, logs JSON with `slog`, drains on `SIGTERM` |
| `internal/gate/` | The gate's three answers, the webhooks that carry them, what it does between questions (`tend.go`), and how it reads the cluster |
| `internal/server/` | The HTTP surface: `/healthz`, `/readyz`, the service's own routes, graceful shutdown |
| `cmd/collector/` | The ClusterState Collector binary: captures at start and on its own interval |
| `internal/collector/` | What the Collector captures, the snapshot it writes, and the envtest case that runs it against a cluster |
| `internal/githubapp/` | A GitHub App installation: one file of one repository, read and written, and the Deployments of every repository it is installed on |
| `internal/loop/` | A pass at start and on an interval, and the freshness probe that says one succeeded lately: the Collector's and the Reporter's |
| `cmd/reporter/` | The Reporter binary: a pass at start, on every event Flux posts it, and on its own interval |
| `internal/reporter/` | The stage each Rollout is at, read from its unit, its source and its Canaries; the writes to its Deployment; and the envtest case that runs it against a cluster |
| `cmd/vault-policy/` | The Vault policy job's binary: runs once and exits |
| `cmd/postgres-catalog/`, `internal/catalog/` | The catalog method's binary, and what it applies: databases, roles, adoption and the Secret Store's database roles |
| `cmd/backup/`, `backup.Dockerfile` | The backup methods' one binary, `backup files\|postgres\|rabbitmq`, and the three images built from it, one target each |
| `internal/backup/` | Writing one generation atomically, pruning to `BACKUP_RETAIN` and a Rollback's copies by age, and what each method writes: a gzipped tar of `/data`, a gzipped `pg_dumpall`, the broker's definitions |
| `internal/vaultpolicy/` | Reading the rendered documents, the six Vault calls, and the run that writes what Vault does not hold as rendered |
| `internal/deploykit/` | Go types generated from deploy-kit's published JSON Schemas, one package per schema, and the test that holds them to deploy-kit's corpus |
| `third_party/deploy-kit/` | The schemas, their accept/refuse corpus and the spec files the corpus reads, vendored at `DEPLOY_KIT_REF`; mirrors deploy-kit's `spec/v1/` |
| `scripts/sync-schemas.sh` | Fetches the vendored files from deploy-kit at one release |
| `deploy/delivery.project.yml` | The `delivery` project's deploy-kit Project Intent: Flagger, the Release Gate, the Collector and the Reporter, each with the Kubernetes API access it declares |
| `deploy/env/collector/base.env` | What the Collector's Process receives: the cluster it captures, the repository it commits to, and its GitHub App |
| `deploy/env/reporter/base.env` | What the Reporter's Process receives: Flux's namespace, and its GitHub App |
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

## The Release Gate

Flagger asks the gate three questions through each Canary's webhooks
(deploy-kit `spec/v1/55-delivery.md#the-release-gate`). Each carries the Application, the Process
and the Application revision, and the gate reads the rest from the cluster.

| Webhook | Path | Yes when |
|---------|------|----------|
| `confirm-rollout` | `POST /may-start` | every step of the release has completed: its migration and its prepare Processes, each a Job named for the revision. An Application that has never run one starts at once |
| `rollout` | `POST /checks` | the member's new copy has a pod, every one is ready, and none has restarted |
| `confirm-promotion` | `POST /may-promote` | every member of the Application has passed its analysis for this revision or did not change in it: the barrier |

Flagger reads a 2xx as yes and anything else as no, so the status is the answer: `200` yes, `409`
not yet, with what it waits on, `400` a payload that asks nothing, `403` a caller that is not
Flagger, `503` a question the gate could not answer. Flagger records a refusal's words on the Canary.

- **A migration the inputs name is a step from the start.** Where the inputs carry a
  `migration`, `may-start` waits for its Job of this revision whether or not that Job is applied
  yet.
- **It decides from the cluster.** Every answer is read when it is asked: the Application's
  `<application>-release-gate` ConfigMap, its Jobs, its members' Canaries and Deployments, the
  new copy's pods.
- **The one thing it remembers only ever withholds a yes.** Flagger's status carries no revision,
  so a member read as waiting for promotion may be waiting from the release before. The gate
  notes the revision each member last asked at, and counts a waiting member only once it has
  asked at this one. A gate that restarts has no notes, and the barrier opens again as each
  waiting member asks, which Flagger has it do on every tick.
- **It fails closed.** A ConfigMap that is missing or does not parse, a Process that is no member
  of it, a Canary of another Application, a list it cannot read: each is a `503`, and the cause
  goes to the log, not the reply.
- **It answers Flagger and nobody else.** Flagger sends no credential a webhook could carry, so
  the gate goes by where the connection comes from: the address of one of the `flagger` Process's
  pods in `delivery-system`, read from the cluster on each request. Only a pod that holds its
  address now counts: running, not ending, not on the host's network, and under Flagger's own
  ServiceAccount. Anyone else gets one `403`
  whatever they ask, before anything of the request or the cluster is read. A forwarded-for
  header is not read. The `delivery` project's derived policy says the same at the network:
  Flagger's edge to the gate is the only one declared for its webhooks.
- **It serves what it holds, read-only, at `GET /releases`.** For every gated Application,
  ordered by namespace and Application: its members and its migration, from the release-gate
  inputs, and its record (`serving`, `pinned`, `since`). An Application whose inputs or record do
  not read says so in `unreadable` and keeps the rest from failing with it. A thousand
  Applications at most, with `truncated` set beyond that. This is how the estate dashboard reads
  both without any ConfigMap access of its own (JorisJonkers-dev/estate-dashboard#15): it declares
  an edge to the gate's `http` surface, which its derived policy then admits. Reading it starts
  and writes nothing, so it checks no caller; and because it checks none, one read answers it for
  15 seconds and only one read runs at a time, so asking it often costs the API server, and the
  client Flagger's webhooks share, nothing more.
- **It answers only at the revision the asking member's Canary carries.** A question about a
  render that has been replaced, or about a Canary that is not there, is refused, and the two
  read the same.
- **A release's steps are read off its Jobs.** The inputs do not name them, so every identity
  that has ever run a release Job of the Application is a step, and its Job of this revision must
  be there and complete. A Job that is not applied yet is a step not done, never a step the
  release lacks. The Job that undoes a migration is no step. The inputs name the migration, so
  a first migration is a step before its Job exists; a first prepare Process is still the one
  case this cannot see (JorisJonkers-dev/deploy-kit#240).
- **The barrier is read off the cluster, not taken on trust.** A member has passed when its
  Canary waits for promotion or is past it. A member "did not change" when Flagger records it as
  serving what it last saw and its primary runs what its Deployment holds: the same images,
  commands, arguments and variables. Flagger's record alone is a tick behind a Deployment that
  just changed. The member that asks is held to its own record: waiting, or analysed with every
  iteration counted.
- **`error-rate` and `latency` are not measured yet.** A blue-green copy receives no user traffic
  before it is promoted, so its own request metrics hold only probes. Until a traffic source and
  a metric source are decided (JorisJonkers-dev/delivery#12), a member that carries those checks
  is held to the same two facts as one that does not.
- **It starts no prepare Process yet.** deploy-kit renders none, so there is no Job to start.

### Between questions

Once a release has failed, Flagger asks nothing more. So every 15 seconds the gate looks at every
gated Application itself (`internal/gate/tend.go`): each ConfigMap the render wrote under the name
`<application>-release-gate`, in whatever namespace.

- **It records what an Application serves.** When Flagger records every member as settled and
  every primary runs what its Deployment holds, the Application serves the revision its Canaries
  carry. The gate writes that to `<namespace>.<application>-release-record`, a ConfigMap of its
  own in `delivery-system`, with the Application's namespace and Flagger's digest of each primary. It
  keeps the record in its own namespace because anything that can write a ConfigMap beside the
  Application could otherwise say which revision serves; a record naming another namespace is
  one it cannot answer from. No rendered object says what
  serves once a new render is applied, so this is the gate's one piece of state, and it survives
  a restart. When it first sees a revision applied and not yet serving, it notes that and when.
- **It starts a migration only while its proof holds.** The up Job, `<identity>-<tag>`, is
  unsuspended when the record's serving revision is the inputs' `testedAgainst` and no primary's
  digest has moved since. A release proven against nothing starts only where nothing is recorded
  as serving. Otherwise the release is held with the schema untouched.
- **It undoes a migration only under every condition of the Down.** The down Job,
  `<identity>-down-<tag>`, is unsuspended when the release is held, the migration ran, every
  member's new copy has no pod left, every primary still runs the proven revision, and
  `nonTransactional` is false. A first release has no down. Where a condition fails, or the Down
  itself failed, nothing is undone and the gate says which.
- **A release is held** when its proof is stale, its migration failed, or Flagger failed a
  member after the gate first saw the revision. Flagger's status carries no revision, so a
  failure from before that is the release before's.
- **A round it cannot finish for one Application does not stop the next**, and that Application
  is reported as unanswerable.

Each round says in the log, as JSON, every held release (`release held`: serving, pinned and the
reason, `stale-proof`, `migration-failed` or `analysis-failed`), every migration it leaves
applied (`migration not undone`: the tag the Down would return to and the condition,
`first-release`, `new-copy-running`, `primaries-moved`, `non-transactional` or `down-failed`),
and every Application it could not read.

Not here yet: a release that outlives the gate deadline is not reported as held, because nothing
says when that deadline starts (JorisJonkers-dev/deploy-kit#264); and there is no `/metrics` for
alert rules to read. A scrape endpoint names namespaces, Applications and revisions, so it waits
until JorisJonkers-dev/delivery#22 decides who may read it and how the gate admits them, the way
the webhooks admit Flagger alone.

Its grant is `get` on Canaries and Deployments, `list` on pods, `list` and `patch` on Jobs, and
`get`, `list`, `create` and `update` on ConfigMaps. A ClusterRole cannot say "only the record"
or "only `suspend`", so the grant is wider than what the gate does with it
(JorisJonkers-dev/delivery#21): it writes one ConfigMap per Application in its own namespace,
named `<namespace>.<application>-release-record`, and `suspend` on a Job it starts. It starts only a Job the
render wrote (`app.kubernetes.io/managed-by: deploy-kit`) under the Application's own migration
identity, `<application>-migration`. A start names the version of the Job the gate read, so a Job put in its
place since is not the one started. A record is named for the namespace as well as the Application,
so inputs under one Application id elsewhere can neither claim it first nor write over it: they
are tended as their own namespace's, and silence nothing of this one. The `api` block of its Process in `deploy/delivery.project.yml` declares the grant, and
deploy-kit renders the ClusterRole, its binding and the token from that.

## The catalog method

deploy-kit renders, for a Postgres whose engine names `catalog: postgres-catalog`, a Job and an
hourly CronJob beside it, handed the catalog as `CATALOG_FILE`
([deploy-kit spec/v1/16-dependencies.md](https://github.com/JorisJonkers-dev/deploy-kit/blob/main/spec/v1/16-dependencies.md#applying-the-catalog)).
Each run, for each database the catalog names, in order:

1. creates the owner and data roles, without login, where they are absent;
2. creates the database, owned by the owner role, where it is absent;
3. adopts a database another role owns: reassigns that role's objects to the owner role and makes
   it a member of it, so old logins keep working; a database a superuser owns fails the run;
4. reassigns what every adopted role has created since;
5. grants the data role read and write on every table and sequence of the owner's schemas, now
   and by default;
6. writes `database/roles/<owner>` and `database/roles/<data>`, whose logins are members of the
   role and act as it on sign-in.

It signs in to Vault as `VAULT_ROLE`, reads the admin login at the catalog's `admin` path, and
never drops anything. A second run changes nothing.

## The Vault policy job

deploy-kit renders a Vault policy and a Kubernetes auth role per identity that holds a grant, as
two JSON documents named for what Vault holds them by, `<namespace>-<identity>`, and applies
neither (deploy-kit `spec/v1/30-deliverables.md#vault-configuration-is-rendered-not-applied`).
This job writes them. It is a Job of the `apps-vso-secrets` Reconcile Unit, so it runs before
any Application that holds a grant, and the unit hands it the documents as files.

| Variable | What it is |
|----------|------------|
| `VAULT_ADDR` | Vault's address |
| `VAULT_ROLE` | the Kubernetes auth role it logs in as; defaults to `policy-admin`, a platform fixture |
| `POLICIES_DIR` | where the documents are mounted; defaults to `/policies` |
| `TOKEN_FILE` | its ServiceAccount token; defaults to the pod's own |

- **The render is read whole before Vault is touched.** A directory holding anything that is not
  a document the render writes is refused, and nothing is applied: half a render is not one.
- **It writes no more than a grant derives.** A name has one reading: the Project stands before
  the first `-system-` and the identity after it. A role must bind exactly that ServiceAccount
  in exactly that Project's `<project>-system` namespace, to the one policy of that name, with
  no namespace selector. A policy must grant only on the forms of path the render writes, each
  with the capabilities that form carries and no other: `read` on a kv document under
  `secret/data/` or `secret/metadata/`, `read` on `database/creds/<role>`, `update` on
  `transit/<sign|verify|encrypt|decrypt>/<key>` and on `transit/keys/<key>/rotate`. No glob, no
  `+`, no template, no `sudo`. So a document that reached the job by another road than the
  render cannot bind another identity, or grant on Vault's own `sys/` and `auth/`. A form the
  render gains is refused until the job's list gains it.
- **Vault parses what the job checked.** The policy written is the job's own encoding of what it
  read, not the file's bytes, so nothing a second parser would read differently reaches Vault.
- **It leaves what is already as rendered alone**, and writes the policy before the role, so a
  role never names a policy that is not there. A role that gained a namespace selector is not
  as rendered: it is written again, without one.
- **It never deletes.** What Vault holds under a name of the render's form, `<project>-system-…`,
  in a namespace its documents are in, that the documents no longer name is logged as stale, for a
  human to remove. Another Project's names are that Project's job's to report, and the platform's
  own fixtures have no such name and are never reported.
- **A call that fails stops the run**, with exit 1, which stops the Reconcile Unit: no
  Application that holds a grant starts against policies Vault does not hold.

deploy-kit renders the Job, and the ConfigMap that carries the documents, where the Platform
document names the job in `policyJob`; both are named by the digest of the documents, so a
changed policy set is a new Job. A grant deploy-kit does not render yet, `custody` and its
prefix, will need the glob this job refuses today.

## The Collector

The Collector captures when it starts and then once every `INTERVAL`, and keeps running: Project
Intent has no schedule, so it keeps its own. Each capture lists PersistentVolumes,
PersistentVolumeClaims, the pods deploy-kit renders and the Jobs it renders for a Rollback's
backup, and compares what they say with
`cluster-state.yml` in the Estate repository
([deploy-kit `spec/v1/20-resolved-deployment.md`](https://github.com/JorisJonkers-dev/deploy-kit/blob/main/spec/v1/20-resolved-deployment.md#the-collector)):

| fact | read from |
|---|---|
| a binding, `{claim, node}` | a bound claim's name, and the one node its volume's node affinity names under `kubernetes.io/hostname`. A volume any node can mount binds its claim to nothing |
| a placement, `{process, node}` | a pod labelled `app.kubernetes.io/managed-by: deploy-kit`: its `app.kubernetes.io/name` and the node it is scheduled to. A pod that has finished is not a placement |
| a backup, `{request, project, job, outcome, finishedAt}` | a Job labelled `estate.jorisjonkers.dev/rollback-backup` and `app.kubernetes.io/managed-by: deploy-kit`, in a namespace ending `-system`: the request label's value, its namespace without `-system`, its name, and its `Complete` or `Failed` condition and when that became true. A Job still running is not listed, and `backups` is written only when one is |

Facts are ordered and free of duplicates. When they equal the committed snapshot's, the capture
commits nothing, and `capturedAt` stays what it was. When they differ, it writes the file
once, through the Collector's GitHub App, replacing exactly the blob it read. A snapshot that
names another cluster, or that deploy-kit's schema would refuse, is never overwritten.

| variable | value |
|---|---|
| `CLUSTER_NAME` | the cluster, as the snapshot names it |
| `ESTATE_REPOSITORY` | `JorisJonkers-dev/estate` |
| `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID` | the Collector's App and its installation on the Estate repository |
| `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_FILE` | the App's private key, as the Secret Store delivers it: a variable, or a file |
| `INTERVAL` | how often to capture, a Go duration; ten minutes when it is not set |
| `ADDR` | where it serves its probes; `:8080` when it is not set |

A capture that fails is logged, and the next one still runs. Because a capture commits only when
a fact changed, a Collector that stopped and a cluster that stayed still look the same in the
Estate repository. `GET /fresh` tells them apart: it answers 200 while a capture succeeded within
twice the interval and 503 after, and it is the Collector's liveness probe, so one that stopped
succeeding is restarted and shows as restarts.

The Collector is an Application of `deploy/delivery.project.yml`. Its `api` block declares the
four kinds it lists, cluster-wide, which deploy-kit renders into its ClusterRole once the
Platform document admits it; its GitHub App arrives as variables, from a grant on
`secret/data/delivery-system/collector`.

## The Reporter

Every status of a Rollout after `pinned` is the Reporter's
([deploy-kit `spec/v1/55-delivery.md`](https://github.com/JorisJonkers-dev/deploy-kit/blob/main/spec/v1/55-delivery.md#rollouts)).
The composition workflow creates the GitHub Deployment when it moves a Project's pin, and the
pin source carries the same Rollout as annotations on the Project's unit. The Reporter reads
that unit, and writes to the Deployment where the stage moved.

A pass runs when the Reporter starts, on every event Flux's Provider posts to `POST /flux`, and
once every `INTERVAL`. An event is a prompt to look and never evidence: the route is unsigned, so
the Reporter reads nothing from it, not even the unit it names, and reads every unit again. A
forged event costs one pass and writes nothing; a dropped one delays a status by an interval at
most. Flagger sends the Reporter nothing, so the interval is also how often a Canary's progress
is seen.

| stage | written when the cluster shows |
|---|---|
| `in_progress`: applied by Flux; then each member that is not through, `checks n/m`, `waits for the other members`, `promoting` or `waits for Flagger` | the unit's last applied revision is the digest its source is pinned to, and a blue-green member has not finished |
| `success`: `<project> <version> applied by Flux and ready` | the release is applied and the unit has no blue-green member |
| `success`: `<project> <version> promoted` | every member's Canary is settled and its primary serves what its Deployment holds: promoted in this release, or unchanged by it, the same test the Release Gate's barrier makes |
| `failure`: `<member> rolled back: <Flagger's message>` | a member's Canary failed after the Deployment was created |
| `failure`: `Flux could not apply <project> <version>: <reason>: <message>` | the unit is not Ready on this release for any reason but waiting on another unit. Flux retries, so a `success` may follow |

A status is written only when it differs from the Deployment's latest, so a Reporter that
restarts writes nothing twice. A Deployment whose latest status is `success`, `inactive` (a later
release superseded it) or `error` takes nothing more. A Deployment is the Rollout's only when its
payload names the unit's Project and version, so a Project released from the same repository as
another never takes the other's status. Each status keeps the run link the workflow gave the
first one, and names the Rollout's URL as the environment URL where it has one.

| variable | value |
|---|---|
| `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID` | the Reporter's App, `deployments: write` and nothing else, and its installation on every repository that releases to the estate |
| `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_FILE` | the App's private key, as the Secret Store delivers it: a variable, or a file |
| `FLUX_NAMESPACE` | where the units are; `flux-system` when it is not set |
| `INTERVAL` | how often to look, a Go duration; thirty seconds when it is not set |
| `ADDR` | where it serves `/flux` and its probes; `:8080` when it is not set |

`GET /fresh` answers 200 while a pass succeeded within twice the interval, and is the
Reporter's liveness probe, as it is the Collector's.

The Reporter is an Application of `deploy/delivery.project.yml`, beside the Release Gate, which
the Platform document's `delivery.reporter` requires. Its `api` block declares its four reads:
Kustomizations listed, each one's OCIRepository, the Canaries the unit's inventory names, and a
settled member's Deployment and primary. deploy-kit renders that into its ClusterRole once the
Platform document admits it; its GitHub App arrives as variables, from a grant on
`secret/data/delivery-system/reporter`.

## Run it

```bash
mise install    # the pinned toolchain
task check      # lint, schemas:check, gen:check, tests with coverage, build, secret scan
docker build --build-arg APP=release-gate -t release-gate .
docker build --build-arg APP=vault-policy -t vault-policy .
docker build --build-arg APP=collector -t collector .
docker build --build-arg APP=reporter -t reporter .
docker build --file backup.Dockerfile --target file-backup -t file-backup .
```

`task test` fails below 80% statement coverage of hand-written code (`COVERAGE_MIN` in
`Taskfile.yml`); generated files and the fake GitHub the tests run against are left out of that
figure. The Collector's and the Reporter's envtest cases start a real API server, which `task test` fetches once
through `setup-envtest`.
