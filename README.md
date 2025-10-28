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

### Using Executable

Build the project (should have docker and buildx installed):

```bash
make omni-infra-provider-linux-amd64
```

Run the executable:

```bash
_out/omni-infra-provider-linux-amd64 --config config.yaml --omni-api-endpoint https://<account-name>.omni.siderolabs.io/ --omni-service-account-key <service-account-key>
```
