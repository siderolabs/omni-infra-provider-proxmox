# Omni Infrastructure Provider for Proxmox

Can be used to automatically provision Talos nodes in a Proxmox cluster.

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

### Using Docker

```bash
docker run -it -d -v ./config.yaml:/config.yaml ghcr.io/siderolabs/omni-infra-provider-proxmox --config /config.yaml --omni-api-endpoint https://<account-name>.omni.siderolabs.io/ --omni-service-account-key <service-account-key>
```

### Using Executable

Build the project (should have docker and buildx installed):

```bash
make omni-infra-provider-linux-amd64
```

Run the executable:

```bash
_out/omni-infra-provider-linux-amd64 --config config.yaml --omni-api-endpoint https://<account-name>.omni.siderolabs.io/ --omni-service-account-key <service-account-key>
```
