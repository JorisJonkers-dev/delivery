# Changelog

## [0.13.1](https://github.com/JorisJonkers-dev/delivery/compare/v0.13.0...v0.13.1) (2026-10-22)


### Bug Fixes

* **reporter:** report a release pinned again at the same commit on its own Deployment ([#76](https://github.com/JorisJonkers-dev/delivery/issues/76)) ([76dbe3b](https://github.com/JorisJonkers-dev/delivery/commit/76dbe3b38d9394b3da0c65b13f5e6e4de7302574))

## [0.13.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.12.0...v0.13.0) (2026-10-21)


### Features

* **backup:** dump the one database a run names, with pg_dump ([#73](https://github.com/JorisJonkers-dev/delivery/issues/73)) ([84d3ffa](https://github.com/JorisJonkers-dev/delivery/commit/84d3ffa9e2565fa35c814b8f47ab2bed78ab9f09))

## [0.12.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.11.0...v0.12.0) (2026-10-21)


### Features

* **gate:** delete a gated Application's sync objects of an older revision once it serves ([#71](https://github.com/JorisJonkers-dev/delivery/issues/71)) ([feec363](https://github.com/JorisJonkers-dev/delivery/commit/feec363465ed56530989436b68d0ed1f17ff9a52))

## [0.11.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.10.0...v0.11.0) (2026-10-21)


### Features

* the images lock records each image's software version ([#68](https://github.com/JorisJonkers-dev/delivery/issues/68)) ([a959cd7](https://github.com/JorisJonkers-dev/delivery/commit/a959cd7b930d5223a0c1617c3815a8b3499d6be8))


### Bug Fixes

* **deps:** update minor and patch dependencies ([#20](https://github.com/JorisJonkers-dev/delivery/issues/20)) ([49d4f8f](https://github.com/JorisJonkers-dev/delivery/commit/49d4f8fbee1cf0adfe4875a1bee5db00b5020799))

## [0.10.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.9.2...v0.10.0) (2026-10-20)


### Features

* start a member only once the stop-start Processes it requires in its own Project have started ([#66](https://github.com/JorisJonkers-dev/delivery/issues/66)) ([dd46d17](https://github.com/JorisJonkers-dev/delivery/commit/dd46d1700e84ce7d46b7bd14db7f0a8a9959dd2d))

## [0.9.2](https://github.com/JorisJonkers-dev/delivery/compare/v0.9.1...v0.9.2) (2026-10-18)


### Bug Fixes

* hand PATH to pg_dumpall, which finds pg_dump beside itself through it ([#64](https://github.com/JorisJonkers-dev/delivery/issues/64)) ([be12776](https://github.com/JorisJonkers-dev/delivery/commit/be12776c76df96835596ff2310d630982dd47764))

## [0.9.1](https://github.com/JorisJonkers-dev/delivery/compare/v0.9.0...v0.9.1) (2026-10-16)


### Bug Fixes

* **vault-policy:** write a catalog method's own policy ([#61](https://github.com/JorisJonkers-dev/delivery/issues/61)) ([bc51071](https://github.com/JorisJonkers-dev/delivery/commit/bc5107112fec8e55ae4ac01f4e2a3395506c202c))

## [0.9.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.8.0...v0.9.0) (2026-10-16)


### Features

* **postgres-catalog:** apply the database catalog the render hands it ([#59](https://github.com/JorisJonkers-dev/delivery/issues/59)) ([543558d](https://github.com/JorisJonkers-dev/delivery/commit/543558dc081e808ff5faa6ef48e801235c7823e9))

## [0.8.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.7.0...v0.8.0) (2026-10-16)


### Features

* **vault-policy:** start a generated key from the value its old location holds ([#57](https://github.com/JorisJonkers-dev/delivery/issues/57)) ([0c0d499](https://github.com/JorisJonkers-dev/delivery/commit/0c0d499883cfd0d737bb3c025800ce3a4218eddc))

## [0.7.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.6.0...v0.7.0) (2026-10-15)


### Features

* the Vault policy job writes the generated keys Vault lacks ([#55](https://github.com/JorisJonkers-dev/delivery/issues/55)) ([9d260a0](https://github.com/JorisJonkers-dev/delivery/commit/9d260a0a10a0080cd87d4a0198f4ce5f70af33e2))

## [0.6.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.5.0...v0.6.0) (2026-10-14)


### Features

* **gate:** serve every gated Application's inputs and record at GET /releases ([#48](https://github.com/JorisJonkers-dev/delivery/issues/48)) ([c51bece](https://github.com/JorisJonkers-dev/delivery/commit/c51becef90eb4335acc3c7ab9f8af0c49a27536d))

## [0.5.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.4.0...v0.5.0) (2026-10-14)


### Features

* report each Rollout's stages to its GitHub Deployment ([#46](https://github.com/JorisJonkers-dev/delivery/issues/46)) ([ffa85f0](https://github.com/JorisJonkers-dev/delivery/commit/ffa85f07129dd915d165dfa0e61a55abf6d0f2bb))

## [0.4.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.3.0...v0.4.0) (2026-10-14)


### Features

* back up for a Rollback, and report how each Rollback's backup ended ([#44](https://github.com/JorisJonkers-dev/delivery/issues/44)) ([a1f8c9d](https://github.com/JorisJonkers-dev/delivery/commit/a1f8c9d71121dc0bff558122e55b3f03805b1744))

## [0.3.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.2.0...v0.3.0) (2026-10-14)


### Features

* **backup:** dump the peer the render hands a network method ([#34](https://github.com/JorisJonkers-dev/delivery/issues/34)) ([b66ee6a](https://github.com/JorisJonkers-dev/delivery/commit/b66ee6a7768406c9eb8af0944e02028b55746515))


### Bug Fixes

* **deploy:** let Flagger read MetricTemplates and AlertProviders ([#42](https://github.com/JorisJonkers-dev/delivery/issues/42)) ([d2834ab](https://github.com/JorisJonkers-dev/delivery/commit/d2834ab27d86ab005e426db7b1407e5ab508d9da))

## [0.2.0](https://github.com/JorisJonkers-dev/delivery/compare/v0.1.1...v0.2.0) (2026-10-07)


### Features

* build the backup method images the Platform document's engines name ([#31](https://github.com/JorisJonkers-dev/delivery/issues/31)) ([50ed4c4](https://github.com/JorisJonkers-dev/delivery/commit/50ed4c433c4956c1ff7936879af0f9ee9b887dcf))

## [0.1.1](https://github.com/JorisJonkers-dev/delivery/compare/v0.1.0...v0.1.1) (2026-10-07)


### Bug Fixes

* lock Flagger in the release's images lock share ([#29](https://github.com/JorisJonkers-dev/delivery/issues/29)) ([3b72521](https://github.com/JorisJonkers-dev/delivery/commit/3b7252114e5620e6dccb7babe92df3a7dbdc2fa7))

## 0.1.0 (2026-10-07)


### Features

* answer Flagger's three webhooks: may-start, checks and the may-promote barrier ([#13](https://github.com/JorisJonkers-dev/delivery/issues/13)) ([d183612](https://github.com/JorisJonkers-dev/delivery/commit/d1836127c1a15b06ef0d2d3681d99f30916f92cd))
* declare each service's Kubernetes API access and the Collector, on the deploy-kit 0.4.0 schemas ([#18](https://github.com/JorisJonkers-dev/delivery/issues/18)) ([bb82a9f](https://github.com/JorisJonkers-dev/delivery/commit/bb82a9f5e0c29c1d10e2cda03537f65832e3a18c))
* publish the release-gate, collector and vault-policy images on each release ([#26](https://github.com/JorisJonkers-dev/delivery/issues/26)) ([dbd28dd](https://github.com/JorisJonkers-dev/delivery/commit/dbd28dd76b87afb22fc80edb5060d202a42ce015))
* publish this repository's Intent Fragment on each release ([#28](https://github.com/JorisJonkers-dev/delivery/issues/28)) ([d9da391](https://github.com/JorisJonkers-dev/delivery/commit/d9da391fe799e09143c41f27a9dabfc6c323f127))
* scaffold the delivery project and generate Go types from deploy-kit's schemas ([#7](https://github.com/JorisJonkers-dev/delivery/issues/7)) ([847cb02](https://github.com/JorisJonkers-dev/delivery/commit/847cb021bdcfa656d38c0868aecaa971f66440fd))
* start and undo migrations from the gate's inputs, record what serves, and report held releases ([#23](https://github.com/JorisJonkers-dev/delivery/issues/23)) ([f8bc6a1](https://github.com/JorisJonkers-dev/delivery/commit/f8bc6a1b84e4857b1c39b38e9b1581802c88e2a1))
* the ClusterState Collector, and the grant its ServiceAccount is held to ([#10](https://github.com/JorisJonkers-dev/delivery/issues/10)) ([695236e](https://github.com/JorisJonkers-dev/delivery/commit/695236ec5c6e9ccf1bf510fd4abc1e367d5ad23c))
* the Collector captures on its own interval, and says whether a capture succeeded lately ([#16](https://github.com/JorisJonkers-dev/delivery/issues/16)) ([ca5a7cb](https://github.com/JorisJonkers-dev/delivery/commit/ca5a7cb005a718a6b1d78eda8f8c0d382524a182)), closes [#15](https://github.com/JorisJonkers-dev/delivery/issues/15)
* the Vault policy job, which writes the rendered policies and auth roles into Vault ([#14](https://github.com/JorisJonkers-dev/delivery/issues/14)) ([4aa3774](https://github.com/JorisJonkers-dev/delivery/commit/4aa3774e210f34add6bfc1633c909ac77cd2632b))
