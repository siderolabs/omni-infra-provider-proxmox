# Omni Infrastructure Provider for Proxmox

Can be used to automatically provision Talos nodes in a Proxmox cluster.

## Requirements

- Proxmox VE cluster
- User account with sufficient permissions to manage VMs and resources (example uses root)
- Omni account and infrastructure provider key
- Network connectivity between the infrastructure provider and your Proxmox cluster

## Running Infrastructure Provider

Create the configuration file for the provider:

```yaml
proxmox:
  username: root
  password: 123456
  url: "https://homelab.proxmox:8006/api2/json"
  insecureSkipVerify: true
  realm: "pam"
```

> **Note:**
>
> - Replace the `url` value with the address of your own Proxmox server.
> - You can use a different user instead of `root` if you grant it the necessary permissions to manage resources in your Proxmox cluster.

### Using Docker

> **Note:** The `--omni-service-account-key` flag expects an *infra provider key*, not an Omni service account key.
> Make sure to provide the correct key type.

Run the provider using Docker:

```bash
docker run -it -d \
  -v ./config.yaml:/config.yaml \
  ghcr.io/siderolabs/omni-infra-provider-proxmox \
  --config-file /config.yaml \
  --omni-api-endpoint https://<account-name>.omni.siderolabs.io/ \
  --omni-service-account-key <infra-provider-key>
```

### Example Docker Compose

You can also run the provider using Docker Compose.
Create a `docker-compose.yaml` file:

```yaml
services:
  omni-infra-provider-proxmox:
    image: ghcr.io/siderolabs/omni-infra-provider-proxmox
    volumes:
      - ./config.yaml:/config.yaml
    command: >
      --config-file /config.yaml
      --omni-api-endpoint https://<account-name>.omni.siderolabs.io/
      --omni-service-account-key <infrastructure-provider-key>
    restart: unless-stopped
```

#### Injecting a Trusted CA Certificate (self-hosted Omni)

If your Omni instance uses a self-signed or internal CA, the Talos node must trust
it before establishing the SideroLink connection.
Set the `TRUSTED_ROOTS_CONFIG_PATH` environment variable to point to a file
containing a Talos `TrustedRootsConfig` document.
The provider will append it to the nocloud `user-data` as a separate YAML document
so it is applied on first boot, before any connection to Omni.

Example CA file (`trusted-roots.yaml`):

```yaml
apiVersion: v1alpha1
kind: TrustedRootsConfig
name: internal-ca
certificates: |-
    -----BEGIN CERTIFICATE-----
    <base64-encoded DER>
    -----END CERTIFICATE-----
```

Extended Docker Compose example with CA injection:

```yaml
services:
  omni-infra-provider-proxmox:
    image: ghcr.io/siderolabs/omni-infra-provider-proxmox
    volumes:
      - ./config.yaml:/config.yaml
      - ./trusted-roots.yaml:/trusted-roots.yaml:ro
    environment:
      TRUSTED_ROOTS_CONFIG_PATH: /trusted-roots.yaml
    command: >
      --config-file /config.yaml
      --omni-api-endpoint https://<account-name>.omni.siderolabs.io/
      --omni-service-account-key <infrastructure-provider-key>
    restart: unless-stopped
```

When `TRUSTED_ROOTS_CONFIG_PATH` is empty or unset the provider behaves exactly as before.

Start the provider:

```bash
docker compose up -d
```

## Creating a Machine Class for Auto Provision

To enable automatic provisioning of Talos nodes, you need to define a machine class of type `auto-provision` in Omni.
This class specifies the configuration for new VMs, such as CPU, memory, and disk size.

Example machine class definition:

```yaml
apiVersion: infrastructure.omni.siderolabs.io/v1alpha1
kind: MachineClass
metadata:
  name: proxmox-auto
spec:
  type: auto-provision
  provider: proxmox
  config:
    cpu: 4
    memory: 8192 # in MB
    diskSize: 40 # in GB
    # Add other Proxmox-specific options as needed
```

Apply the machine class to your Omni account using the Omni UI or CLI.

### Scaling a Cluster with the Machine Class

You can now use above `proxmox-auto` machine class to scale an existing cluster up or down, or to create a new cluster:

- **To scale up:** Increase the desired number of machines in your cluster configuration.
  Omni will automatically provision new VMs using the specified machine class.
- **To scale down:** Decrease the desired number of machines.
  Omni will remove excess VMs accordingly.
- **To create a new cluster:** Specify the machine class in your cluster manifest when creating a new cluster.

Example cluster manifest snippet:

```yaml
spec:
  machineClass: proxmox-auto
  replicas: 3
```

### Storage Selector Requirement During VM Sync

> **Note:**
> During the `vmSync` step, you may encounter an error requiring a Storage Selector.
> This is a CEL (Common Expression Language) expression used to select the appropriate Proxmox storage for VM disk images.
>
> To resolve this, add a `storageSelector` field to your machine class configuration.

```yaml
config:
  ...
  storageSelector: 'name == "local-lvm"'
```

Replace `"local-lvm"` with the name of the storage you want to use for VM disks in your Proxmox cluster.

### USB Device Passthrough

USB devices can be attached through Proxmox Resource Mappings.
Define the mapping under **Datacenter → Resource Mappings → USB Devices**, then reference its name in the machine class:

```yaml
config:
  ...
  usb_devices:
    - mapping: rtl-sdr
      usb3: true
    - mapping: zigbee-controller
```

Devices are assigned to `usb0`, `usb1`, and subsequent slots in list order.
When a machine can run on multiple Proxmox nodes, define each mapping on every
eligible node.

### High Availability

Adding an `ha:` block to the machine class registers each provisioned VM as a Proxmox HA resource
and maintains node-affinity / resource-affinity rules per machine request set
(requires Proxmox VE 9+):

```yaml
config:
  ...
  ha:
    state: started
    resource_affinity: negative # spread the set's VMs across nodes
    node_affinity_nodes:
      - pve1
      - pve2
```

When `ha:` is set, node placement is delegated to Proxmox HA and the provider's
client-side spread is disabled.
For dynamic rebalancing, enable the cluster resource scheduler in `datacenter.cfg`
(`crs: ha=dynamic`, Proxmox VE 9.2+).
See the [Proxmox HA manager documentation](https://pve.proxmox.com/pve-docs/chapter-ha-manager.html).

### Using Executable

Build the project (should have docker and buildx installed):

```bash
make omni-infra-provider-linux-amd64
```

Run the executable:

```bash
_out/omni-infra-provider-linux-amd64 --config config.yaml --omni-api-endpoint https://<account-name>.omni.siderolabs.io/ --omni-service-account-key <service-account-key>
```

## Running Integration Tests

End-to-end tests spin up two Proxmox VE nodes in privileged Docker containers
(via [containerized-proxmox](https://github.com/LongQT-sea/containerized-proxmox)),
form a `pvecm` cluster between them, launch Omni and the provider, then drive the
`omni-integration-test` suite against the provider.

Requirements on the host running the tests:

- Linux kernel 6.8+ with `/dev/kvm` (Intel VT-x or AMD-V enabled)
- Docker 26+ with privileged containers permitted
- `sops` installed and a decryptable `.secrets.yaml` providing `AUTH0_TEST_USERNAME`, `AUTH0_CLIENT_ID`, `AUTH0_DOMAIN`

Run it via:

```bash
sudo -E make run-integration-test
```

The Proxmox containers, Vault, and Omni are torn down on exit (in CI they are
left in place so the artifact upload steps can collect logs from
`/tmp/proxmox-e2e/`).
