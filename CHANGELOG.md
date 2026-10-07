# Changelog

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
