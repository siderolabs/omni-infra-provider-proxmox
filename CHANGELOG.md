## [Omni Infra Provider Proxmox 0.3.0](https://github.com/siderolabs/omni-infra-provider-proxmox/releases/tag/v0.3.0) (2026-09-16)

Welcome to the v0.3.0 release of Omni Infra Provider Proxmox!



Please try out the release binaries and report any issues at
https://github.com/siderolabs/omni-infra-provider-proxmox/issues.

### Installation Media Through Omni

Previously, the provider built the image factory URL itself. This only worked as long as the downloads stayed anonymous.

Omni now resolves the installation media and returns a URL which the Proxmox node can download on its own. This way, an image factory that authenticates its downloads works as well.

The ISO is stored under a name which stays the same when the credentials are rotated, so rotating them does not leave an unused ISO behind on the node.


### USB Device Passthrough

Machines can now be provisioned with USB devices passed through from the Proxmox node. The devices are declared as Proxmox resource mappings.

Each device is assigned to a fixed USB slot, and can optionally use the USB 3 controller.


### Contributors

* Utku Ozdemir
* Artem Chernyshev
* Mitch Ross
* Oguz Kilcan

### Changes
<details><summary>5 commits</summary>
<p>

* [`fec0866`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/fec0866db8c8a2ef44886ff991d2d0c793c54e4c) test: run the integration tests against the enterprise image factory
* [`2276ac7`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/2276ac77fa817f40fab5d4201a6fefd9c3d8e6d4) chore: rekres
* [`7cefedd`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/7cefeddbf2145bc400e3b529e6d5dfe08c27194a) feat: resolve the ISO through Omni's installation media API
* [`d5ac04d`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/d5ac04dd8367bcc155e86fe1c1264f7e98281b79) feat: add USB device passthrough
* [`8b085a2`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/8b085a264265303f6d5605c719320380987aebe4) fix: improve checks for nodes removed bypassing the infra provider
</p>
</details>

### Dependency Changes

* **github.com/luthermonson/go-proxmox**         v0.7.1 -> v0.8.1
* **github.com/planetscale/vtprotobuf**          ba97887b0a25 -> 8ae5a48058df
* **github.com/siderolabs/omni/client**          582730ce940c -> b1341200b16d
* **github.com/siderolabs/talos/pkg/machinery**  v1.14.0-alpha.2 -> 322de8bf2974
* **github.com/stretchr/testify**                v1.11.1 -> v1.12.1
* **go.yaml.in/yaml/v4**                         v4.0.0-rc.6 -> 643e93b9c9be
* **google.golang.org/protobuf**                 f2248ac996af -> v1.36.12

Previous release can be found at [v0.2.0](https://github.com/siderolabs/omni-infra-provider-proxmox/releases/tag/v0.2.0)

## [Omni Infra Provider Proxmox 0.2.0](https://github.com/siderolabs/omni-infra-provider-proxmox/releases/tag/v0.2.0) (2026-07-23)

Welcome to the v0.2.0 release of Omni Infra Provider Proxmox!



Please try out the release binaries and report any issues at
https://github.com/siderolabs/omni-infra-provider-proxmox/issues.

### Firmware Display Options

Added options to control how machine firmware is displayed.


### Bug Fixes and Improvements

- Fixed the scheduler failing to release its reservation on deprovision.
- Fixed node selection clumping onto a single node and causing OOM on non-exclusive Proxmox clusters.
- Fixed ISO images not being re-downloaded when a prior download task failed.
- Various dependency updates, including go-proxmox.


### Proxmox HA Support

Machines can now be registered with Proxmox HA, with the provider managing HA resources and resource-affinity rules for provisioning and teardown, including tolerating Proxmox rejecting an infeasible affinity rule.


### Node Placement Strategies

Added configurable placement strategies for node selection when provisioning machines across a Proxmox cluster, declared as an enum in the provider schema.


### Add Versioning Support

The infra provider will now report its version to Omni.


### Contributors

* netshad0w
* Adrian Popescu
* Artem Chernyshev
* Edward Sammut Alessi
* Mitch Ross

### Changes
<details><summary>17 commits</summary>
<p>

* [`9510c5a`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/9510c5a55f9c24684ca9023720912c422e88949c) feat: rekres and add versioning
* [`fd21629`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/fd21629a87da0af2657613b137301c374d7c87d7) feat: add firmware display options
* [`1c6b3bf`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/1c6b3bfd0380b7cbacd25fe744c07803b4c253b8) feat: declare placement_strategy in provider schema as enum
* [`220dc87`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/220dc87bb08ba06b21622e4ad8df931cecdd0ee6) fix: defer placement_strategy parse and clamp memory math
* [`a1c1814`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/a1c1814a49cd0a2162ddfb0bd33f97a4a9d15a16) feat: add placement strategies for node selection
* [`1eadd18`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/1eadd18836e464d6db08903b13bdb5a2982dffca) fix: tolerate proxmox rejecting an infeasible resource-affinity rule
* [`92255ee`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/92255ee180ffc0eb52650327fad89a678053a360) refactor: drop redundant ha config validation
* [`e1b0959`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/e1b09594879deb15e2b213aa0afa1f377579f78e) test: add an HA-mode proxmox integration test pipeline
* [`6dcb57a`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/6dcb57af5b678908388461a81e8106dd64710878) feat: provision and tear down proxmox HA from the machine class
* [`559cf90`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/559cf9040fa3e53f8c8c2acf01f0e19a72214188) feat: add proxmox HA resource and affinity-rule manager
* [`3ce2684`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/3ce2684788bc802b429eaa5cdd22549e8163d74d) feat: add ha_registered field to the machine spec
* [`87a3a80`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/87a3a80393e9d82d98fd631b98f24fc67324b860) chore: bump go-proxmox to v0.7.1
* [`8bcce21`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/8bcce21731897b3dca78eba5962fc750cd42c0a3) fix: release scheduler reservation on deprovision
* [`346b9ec`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/346b9ec0922a586fba240573b2ea736544513f81) test: extract repeated node names and presize picked slice
* [`22bb362`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/22bb362286435bb4b1c0c9792141b9b4686fcf30) fix: stop clumping one node into OOM on non-exclusive pmx clusters
* [`4929213`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/4929213743fab2d6a9d72274f8f72381aac35284) fix: re-download the ISO image if detected that the task is failed
* [`ade698e`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/ade698e58055963b3892b67d5ff2ff4b6bd74fda) feat: bump dependencies
</p>
</details>

### Dependency Changes

* **github.com/cosi-project/runtime**            v1.14.1 -> v1.16.2
* **github.com/google/cel-go**                   v0.28.0 -> v0.28.1
* **github.com/luthermonson/go-proxmox**         v0.4.1 -> v0.7.1
* **github.com/siderolabs/omni/client**          v1.6.5 -> 582730ce940c
* **github.com/siderolabs/talos/pkg/machinery**  v1.13.0-beta.1 -> v1.14.0-alpha.2
* **go.uber.org/zap**                            v1.27.1 -> v1.28.0
* **go.yaml.in/yaml/v4**                         v4.0.0-rc.4 -> v4.0.0-rc.6

Previous release can be found at [v0.1.0](https://github.com/siderolabs/omni-infra-provider-proxmox/releases/tag/v0.1.0)

## [Omni Proxmox Infra Provider 0.1.0](https://github.com/siderolabs/omni-infra-provider-proxmox/releases/tag/v0.1.0) (2026-05-22)

Welcome to the v0.1.0 release of Omni Proxmox Infra Provider!



Please try out the release binaries and report any issues at
https://github.com/siderolabs/omni-infra-provider-proxmox/issues.

### Contributors

* Artem Chernyshev
* Arnold Mendez
* Brent
* CppBunny
* Mitch Ross
* Moritz Renker
* Tyler Ault
* netshad0w

### Changes
<details><summary>22 commits</summary>
<p>

* [`8e3dd5d`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/8e3dd5dabf7be48368a939b6b81fe0e7dad707ef) feat: configurable firewall on primary NIC via network_firewall
* [`eec4f31`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/eec4f319dcd3474729b2fba2524a145bf4080c79) feat: skip offline nodes during provisioning to prevent API errors
* [`23ace4a`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/23ace4a451567fda34b61de98eba1b3bfe93f680) feat: support vm tags and pool in machine class config
* [`144fd6c`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/144fd6c859825a33ae9ba125dffda12335418295) fix: deduplicate ISO uploads
* [`a4187e1`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/a4187e1e697d543e0dd194969d72fa6b64f14c36) test: add integration tests
* [`f5a527c`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/f5a527c2a86347907342847331f58e7968b1defb) chore: bump deps
* [`eb6f533`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/eb6f533257cbec905963bad6a80df6d83325ca6f) feat: add support for pcie mdev
* [`0d9fd58`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/0d9fd58da9ec9de75506938c755a03f4ade28ee8) feat: make provider automatically distribute VMs across nodes
* [`559954c`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/559954c759bd5b2cf319bcc0ac8c974bdb6621bb) feat: use nocloud images instead of metal
* [`e3a4a2f`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/e3a4a2f29c58f5d04845761c39fef5160b8a09cc) feat: build multiarch docker image for the provider
* [`fb8e4b2`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/fb8e4b28ef1a89d14822222cb40c1ace30e2e170) feat: honor node field in providerdata
* [`7058f00`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/7058f00a1daca77f0abc7e5e239a4e6993bdb8b1) feat: add advanced vm options
* [`5056bf8`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/5056bf8225d1e8e10cf89ecc1d8bb33d9573bd34) feat: install `qemu-guest-agent` on each machine created by provider
* [`f1daa55`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/f1daa556c693e258d3fd22da67fd0ac5810919e4) fix: use unique patch name for the hostname patches
* [`e7248ed`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/e7248ed49a700628b3120cc155039227eb118d49) chore: use machine request id as a hostname for the created nodes
* [`755fa1d`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/755fa1d1ca2ab0b45c4a5aa5eb959891d8058faf) fix: ignore not found machines during deprovisioning
* [`c71a31b`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/c71a31bff1c3406d45f9c9656c49a6d193d6aa41) feat(networking): allow customization of VM networking
* [`7e393fd`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/7e393fd1026f494976256e95eca0c005d73ecdb8) fix: bump Omni client library
* [`8cd7603`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/8cd76037d3484d732d03f83e86d88e99165a627a) fix: make `storage_selector` required
* [`b663c40`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/b663c40c207ecaeb4fee8f136c864f664710b682) docs: extend readme with reqs, docker compose, and setup helpers
* [`410ed22`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/410ed2289e2621eafc69d7c2a68ce14739ffc118) chore: rekres, bump deps
* [`da2f853`](https://github.com/siderolabs/omni-infra-provider-proxmox/commit/da2f8535996ce2db716ca414dda48bab9ffd243e) initial commit
</p>
</details>

### Dependency Changes

This release has no dependency changes

