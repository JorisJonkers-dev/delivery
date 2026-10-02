# Versioning & release

Every JorisJonkers-dev repo is versioned and released the same way. Nothing resolves
or deploys from a moving branch — `main` is an integration branch, not a
deploy target.

## Releasing this repo

1. Land changes via squash-merged PRs with conventional-commit titles
   (`feat:`, `fix:`, `chore:`, `feat!:` / `BREAKING CHANGE:` for majors).
2. `release.yml` (release-please) maintains a release PR. Merging it tags
   `vX.Y.Z`, writes `CHANGELOG.md`, and bumps `.release-please-manifest.json`.
3. The published-release event publishes artifacts at that exact version:
   - Maven libraries/plugins → GitHub Packages under `dev.jorisjonkers.*`
   - npm packages → GitHub Packages under `@jorisjonkers-dev/*`
   - container images → `ghcr.io/jorisjonkers-dev/<repo>/<image>:X.Y.Z`

SemVer: pre-1.0 (`0.y.z`) treats minor as the breaking lever
(`bump-minor-pre-major`). Promote to `1.0.0` once an artifact's API is stable.

## Consuming shared artifacts (exact pins, no ranges)

- **Gradle**: declare versions only in `gradle/libs.versions.toml`; reference
  them via the catalog. Example:
  ```toml
  [versions]
  jorisjonkers-kotlin-commons = "0.3.1"
  [libraries]
  jorisjonkers-command = { module = "dev.jorisjonkers.kotlin-commons:command", version.ref = "jorisjonkers-kotlin-commons" }
  ```
- **npm**: pin exact versions in the manifest (no `^`/`~`).
- **GitHub Actions / reusable workflows**: pin to a release tag (and digest via
  Renovate), e.g. `uses: JorisJonkers-dev/github-workflows/.github/workflows/jvm-ci.yml@v1.2.0`.

[Renovate](renovate.json) opens exact-version bump PRs (JorisJonkers-dev artifacts
grouped into one platform bump). Every bump PR must pass `Pipeline Complete`
before merge.

## Deploying a specific version (apps, e.g. personal-stack)

Deployment is version-pinned and explicit:

1. A release tag builds version-tagged images.
2. A release PR bumps the explicit image tags in the Flux manifests to that
   version. There is no `:latest` and no Keel auto-roll.
3. Deploying a version = reconciling the commit that pins it. **Rollback = `git
   revert`** of the bump.

This makes "which version is live" a reviewable, revertable fact in git.
