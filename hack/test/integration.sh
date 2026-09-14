#!/bin/bash

set -eoux pipefail

TMP="/tmp/proxmox-e2e"
mkdir -p "${TMP}"

# Settings.

TALOS_VERSION=1.13.2
OMNI_VERSION=${OMNI_VERSION:-latest}
IMAGE_FACTORY_ENTERPRISE_ENV=${IMAGE_FACTORY_ENTERPRISE_ENV:-staging} # staging or prod, selects the enterprise factory and its token

case "${IMAGE_FACTORY_ENTERPRISE_ENV}" in
  staging) IMAGE_FACTORY_URL=https://factory-enterprise.staging.talos.dev ;;
  prod) IMAGE_FACTORY_URL=https://factory.siderolabs.com ;;
  *)
    echo "unknown image factory enterprise environment: ${IMAGE_FACTORY_ENTERPRISE_ENV}" >&2
    exit 1
    ;;
esac

IMAGE_FACTORY_TOKEN_VAR="IMAGE_FACTORY_ENTERPRISE_${IMAGE_FACTORY_ENTERPRISE_ENV^^}_TOKEN"
set +x # keep the token out of the trace
IMAGE_FACTORY_TOKEN="${!IMAGE_FACTORY_TOKEN_VAR:?${IMAGE_FACTORY_TOKEN_VAR} must be set}"
set -x
OMNI_IMAGE="ghcr.io/siderolabs/omni:${OMNI_VERSION}"
OMNI_INTEGRATION_TEST_IMAGE="ghcr.io/siderolabs/omni-integration-test:${OMNI_VERSION}"
K8S_VERSION="${K8S_VERSION:-1.35.0}"

ARTIFACTS=_out
JOIN_TOKEN=testonly
PROXMOX_PASSWORD=testonly
RUN_DIR=$(pwd)
PLATFORM=$(uname -s | tr "[:upper:]" "[:lower:]")

# Docker bridge for the Proxmox cluster (must match hack/test/docker-compose.yml).
PROXMOX_NET=proxmox-test
PVE1_IP=10.0.99.1
PVE2_IP=10.0.99.2
DOCKER_GATEWAY=10.0.99.99
CLUSTER_NAME=omni-test

COMPOSE_FILE=hack/test/docker-compose.yml

if [[ "${CI:-false}" == "true" ]] && ! docker compose version >/dev/null 2>&1; then
  COMPOSE_VERSION=v2.32.4
  install -d /usr/local/lib/docker/cli-plugins
  curl -fsSL "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-$(uname -m)" \
    -o /usr/local/lib/docker/cli-plugins/docker-compose
  chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
fi

# Download required artifacts.

mkdir -p ${ARTIFACTS}

OMNICTL="${TMP}/omnictl"

# omnictl refuses to talk to a backend with a different API version, so take it out of the
# Omni image under test rather than from the latest Omni release: ghcr.io/siderolabs/omni:latest
# is built from main and can be several API versions ahead of the last tagged release.
docker pull ${OMNI_IMAGE}

OMNICTL_CONTAINER=$(docker create ${OMNI_IMAGE})
docker cp ${OMNICTL_CONTAINER}:/omnictl/omnictl-linux-amd64 ${OMNICTL}
docker rm -v ${OMNICTL_CONTAINER}

chmod +x ${OMNICTL}

# Build registry mirror args.

if [[ "${CI:-false}" == "true" ]]; then
  REGISTRY_MIRROR_FLAGS=()

  for registry in docker.io k8s.gcr.io quay.io gcr.io ghcr.io registry.k8s.io; do
    service="registry-${registry//./-}.ci.svc"
    addr=$(python3 -c "import socket; print(socket.gethostbyname('${service}'))")

    REGISTRY_MIRROR_FLAGS+=("--registry-mirror=${registry}=http://${addr}:5000")
  done
else
  # use the value from the environment, if present
  REGISTRY_MIRROR_FLAGS=("${REGISTRY_MIRROR_FLAGS:-}")
fi

function cleanup() {
  docker compose -f ${COMPOSE_FILE} logs > ${TMP}/proxmox.log 2>&1 || true

  if [[ "${CI:-false}" == "false" ]]; then
    rm -rf ${TMP}

    docker compose -f ${COMPOSE_FILE} down -t 5 --remove-orphans || true
    docker rm -f omni vault-dev || true
  fi

  # _out/omni holds files written by the Omni container as root; delete via a throwaway
  # container so the host user doesn't need sudo.
  docker run --rm -v "$(pwd)/_out:/data" alpine rm -rf /data/omni || true

  # In CI we run via `sudo -E`, so _out ends up root-owned. Hand it back to the invoking user
  # so subsequent non-root steps (artifact list, upload) can write into it.
  if [[ -n "${SUDO_UID:-}" ]]; then
    docker run --rm -v "$(pwd)/_out:/data" alpine chown -R "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" /data || true
  fi
}

trap cleanup EXIT SIGINT

# Start the two Proxmox nodes first so the docker bridge exists and the nested VMs can be NATed
# out before Omni starts listening for siderolink traffic.

docker compose -f ${COMPOSE_FILE} up -d

# Start Vault.

docker run --rm -d --cap-add=IPC_LOCK -p 8200:8200 -e 'VAULT_DEV_ROOT_TOKEN_ID=dev-o-token' --name vault-dev hashicorp/vault:1.15

sleep 10

# Load key into Vault.

docker cp hack/certs/key.private vault-dev:/tmp/key.private
docker exec -e VAULT_ADDR='http://0.0.0.0:8200' -e VAULT_TOKEN=dev-o-token vault-dev \
    vault kv put -mount=secret omni-private-key \
    private-key=@/tmp/key.private

sleep 5

# Launch Omni in the background.
#
# Omni requires an authentication provider, but nobody logs in during the test, everything goes through
# the initial service account. The Auth0 provider only contacts its domain when a user logs in, so
# placeholder values are enough and no identity provider is needed.
#
# Omni runs with --network host and advertises the docker bridge gateway (${DOCKER_GATEWAY}) to
# joining VMs. Each pve container's vmbr1 is later re-bridged onto its eth0 (the docker network),
# so VMs come up directly on the docker subnet and reach the gateway on the host without NAT.

export BASE_URL=https://localhost:8099/

mkdir -p _out/omni/

# Omni reads the image factory token from a file, placed in the mounted directory that is not uploaded as an artifact.
set +x # keep the token out of the trace
printf '%s' "${IMAGE_FACTORY_TOKEN}" >_out/omni/factory-token
set -x

docker run -it -d --network host -v ./hack/certs:/certs \
    -v $(pwd)/_out/omni:/_out \
    --cap-add=NET_ADMIN \
    --device=/dev/net/tun \
    -e SIDEROLINK_DEV_JOIN_TOKEN="${JOIN_TOKEN}" \
    -e VAULT_TOKEN=dev-o-token \
    -e VAULT_ADDR='http://127.0.0.1:8200' \
    --name omni \
    ${OMNI_IMAGE} \
    --siderolink-wireguard-advertised-addr ${DOCKER_GATEWAY}:50180 \
    --siderolink-api-advertised-url "grpc://${DOCKER_GATEWAY}:8090" \
    --machine-api-bind-addr 0.0.0.0:8090 \
    --siderolink-wireguard-bind-addr 0.0.0.0:50180 \
    --event-sink-port 8091 \
    --advertised-api-url "${BASE_URL}" \
    --auth-auth0-enabled true \
    --auth-auth0-client-id placeholder \
    --auth-auth0-domain auth0.invalid \
    --initial-users test-user@siderolabs.com \
    --private-key-source "vault://secret/omni-private-key" \
    --public-key-files "/certs/key.public" \
    --bind-addr 0.0.0.0:8099 \
    --key /certs/localhost-key.pem \
    --cert /certs/localhost.pem \
    --etcd-embedded-unsafe-fsync=true \
    --create-initial-service-account \
    --sqlite-storage-path db.sql \
    --initial-service-account-key-path=/_out/key \
    --eula-accept-email test-user@siderolabs.com \
    --eula-accept-name "Test User" \
    --primary-factory-url "${IMAGE_FACTORY_URL}" \
    --primary-factory-token-file /_out/factory-token \
    --primary-factory-machine-token-ttl 8h \
    "${REGISTRY_MIRROR_FLAGS[@]}"

docker logs -f omni &> ${TMP}/omni.log &

# Wait for both Proxmox APIs to come up.

function wait_pve_ready() {
  local name="$1"
  local deadline=$(( $(date +%s) + 300 ))

  until docker exec "${name}" pvesh get /version >/dev/null 2>&1; do
    if [[ $(date +%s) -gt ${deadline} ]]; then
      echo "${name} did not become ready in time"
      docker logs "${name}" || true
      return 1
    fi
    sleep 3
  done
}

wait_pve_ready pve-1
wait_pve_ready pve-2

# /etc/hosts: each node must resolve the other for corosync + pvecm.

function setup_hosts() {
  local name="$1"
  docker exec -i "${name}" sh -c "cat >> /etc/hosts" <<EOF
${PVE1_IP} pve-1.local pve-1
${PVE2_IP} pve-2.local pve-2
EOF
}

setup_hosts pve-1
setup_hosts pve-2

# Make `local` storage usable for VM disks: a fresh PVE node omits "images" from `local` by default.

docker exec pve-1 pvesm set local --content iso,vztmpl,backup,images,snippets,rootdir

# The containerized-proxmox image ships vmbr1 as a NAT'd internal bridge (172.16.99.0/24) for
# default-out-of-the-box VM use, and vmbr2 as an empty user-configurable bridge. We use vmbr2
# (the user slot): bridge it onto eth0 so VMs sit directly on the docker subnet, take over
# eth0's IP, set the docker bridge gateway as default route, and pin MTU to 1450 to match the
# CI runner's eth0 so WireGuard packets don't get dropped on egress. We also remove vmbr1
# entirely since we don't use it -- its `up` hooks try to load iptables NAT modules that the
# CI host's Talos kernel doesn't ship, polluting the network reload with warnings.

function configure_node_network() {
  local name="$1"
  local ip="$2"
  docker exec "${name}" pvesh delete "/nodes/${name}/network/vmbr1" || true
  docker exec "${name}" rm -f /etc/dnsmasq.d/vmbr1.conf
  docker exec "${name}" pvesh set "/nodes/${name}/network/vmbr2" \
    --type bridge \
    --bridge_ports eth0 \
    --cidr "${ip}/24" \
    --gateway "${DOCKER_GATEWAY}" \
    --mtu 1450
  docker exec "${name}" pvesh set "/nodes/${name}/network"
}

configure_node_network pve-1 "${PVE1_IP}"
configure_node_network pve-2 "${PVE2_IP}"

# The PVE network reload briefly races with us while eth0's IP moves onto vmbr2; wait for the
# API to come back up.
sleep 3
until docker exec pve-1 pvesh get /version >/dev/null 2>&1 && \
      docker exec pve-2 pvesh get /version >/dev/null 2>&1; do sleep 2; done

# Drop in a dnsmasq config for vmbr2 on pve-1: serve the docker subnet, hand out the docker
# bridge gateway via DHCP option 3 and MTU 1450 via option 26. vmbr2 is bridged onto the docker
# network on both pve nodes, so a single dnsmasq instance on pve-1 sees broadcasts from VMs on
# either node. The image's vmbr1 dnsmasq keeps running but stays on its 172.16.99.0/24 island.
docker exec -i pve-1 tee /etc/dnsmasq.d/vmbr2.conf >/dev/null <<EOF
# Managed by hack/test/integration.sh
interface=vmbr2

dhcp-range=set:vmbr2,10.0.99.10,10.0.99.89,255.255.255.0,12h
dhcp-option=tag:vmbr2,3,${DOCKER_GATEWAY}
dhcp-option=tag:vmbr2,26,1450
EOF
docker exec pve-1 systemctl restart dnsmasq

# Exchange the existing id_rsa pubkeys (pre-generated in the image) so SSH between nodes works
# without password, and pre-seed known_hosts so ssh-copy-id never blocks on a fingerprint prompt.

function setup_ssh() {
  PVE1_PUB=$(docker exec pve-1 cat /root/.ssh/id_rsa.pub)
  PVE2_PUB=$(docker exec pve-2 cat /root/.ssh/id_rsa.pub)

  docker exec pve-1 sh -c "echo '${PVE2_PUB}' >> /root/.ssh/authorized_keys"
  docker exec pve-2 sh -c "echo '${PVE1_PUB}' >> /root/.ssh/authorized_keys"

  docker exec pve-1 sh -c "ssh-keyscan -H pve-2 ${PVE2_IP} >> /root/.ssh/known_hosts 2>/dev/null"
  docker exec pve-2 sh -c "ssh-keyscan -H pve-1 ${PVE1_IP} >> /root/.ssh/known_hosts 2>/dev/null"
}

setup_ssh

# Create the cluster on pve-1, then join pve-2.

docker exec pve-1 pvecm create "${CLUSTER_NAME}" --link0 "${PVE1_IP}"

# pvecm add silently leaves the cluster half-joined if it runs too early: corosync ends up linked
# but the CA cert merge step (which writes to /etc/pve/priv via SSH) is skipped. Retrying after
# the partial join doesn't recover the certs, so cross-node API proxying (used by the Talos ISO
# upload task lookup) fails with TLS errors. Wait until SSH-context writes to pmxcfs actually
# work, then run pvecm add exactly once.

deadline=$(( $(date +%s) + 300 ))
until docker exec pve-2 ssh -o BatchMode=yes -o StrictHostKeyChecking=no \
        root@"${PVE1_IP}" 'echo cluster-ready >> /etc/pve/priv/authorized_keys' >/dev/null 2>&1; do
  if [[ $(date +%s) -gt ${deadline} ]]; then
    echo "pmxcfs on pve-1 did not become writable from an SSH context within deadline"
    docker exec pve-1 pvecm status || true
    exit 1
  fi
  sleep 2
done

docker exec pve-2 pvecm add "${PVE1_IP}" --link0 "${PVE2_IP}" -use_ssh

deadline=$(( $(date +%s) + 60 ))
until docker exec pve-1 pvecm nodes 2>/dev/null | awk '/pve-2/{found=1} END{exit !found}'; do
  if [[ $(date +%s) -gt ${deadline} ]]; then
    echo "pve-2 did not appear in pve-1's node list after pvecm add"
    docker exec pve-1 pvecm status || true
    exit 1
  fi
  sleep 2
done

# Confirm the cert merge worked: pve-1 should now have a copy of pve-2's signed cert, otherwise
# the provider's cross-node API proxy will hit TLS verify failures.
docker exec pve-1 test -f /etc/pve/nodes/pve-2/pve-ssl.pem

docker exec pve-1 pvecm status

# Write the provider config file.

cat > ${TMP}/proxmox-config.yaml <<EOF
proxmox:
  url: https://${PVE1_IP}:8006/api2/json
  username: root
  password: ${PROXMOX_PASSWORD}
  realm: pam
  insecureSkipVerify: true
EOF

# Launch infra provider in the background.
#
# Omni runs from a scratch image, so we can't `docker exec` to read the service-account key.
# _out/omni/key is bind-mounted but written as root, so a host-side `cat` would need sudo.
# Use a throwaway container that shares Omni's volume to wait for and read the key.

deadline=$(( $(date +%s) + 300 ))
until [[ -n "$(docker run --rm --volumes-from omni alpine sh -c '[ -s /_out/key ] && cat /_out/key' 2>/dev/null)" ]]; do
  if [[ $(date +%s) -gt ${deadline} ]]; then
    echo "Omni did not write the service-account key within deadline"
    docker logs omni 2>&1 | tail -50 || true
    exit 1
  fi
  sleep 2
done

export OMNI_ENDPOINT=https://localhost:8099
export OMNI_SERVICE_ACCOUNT_KEY=$(docker run --rm --volumes-from omni alpine cat /_out/key)

${OMNICTL} --insecure-skip-tls-verify infraprovider create proxmox | tail -n5 | head -n2 | awk '{print "export " $0}' > ${TMP}/env

source ${TMP}/env

nice -n 10 ${ARTIFACTS}/omni-infra-provider-proxmox-linux-amd64 \
  --omni-api-endpoint https://localhost:8099 \
  --config-file ${TMP}/proxmox-config.yaml \
  --insecure-skip-verify &

# Provider machine-class config for the auto-provision suite. WITH_HA_MODE appends an ha: block
# so the run also exercises the provider's HA registration and affinity-rule reconciliation. The
# containerized nodes can't run HA at runtime (no hardware watchdog), so this covers the HA
# provisioning path, not failover. Kept single-quoted so the inner \"local\" escaping survives.
PROVIDER_DATA='disk_size: 8, cores: 4, memory: 2048, sockets: 1, network_bridge: vmbr2, storage_selector: "name == \"local\""'

if [[ "${WITH_HA_MODE:-false}" == "true" ]]; then
  PROVIDER_DATA="${PROVIDER_DATA}, ha: {node_affinity_nodes: [pve-1, pve-2], resource_affinity: positive}"
fi

docker run \
  -v $(pwd)/hack/certs:/etc/ssl/certs \
  -e SSL_CERT_DIR=/etc/ssl/certs \
  -e OMNI_SERVICE_ACCOUNT_KEY=$(docker run --rm --volumes-from omni alpine cat /_out/key) \
  --network host \
  ${OMNI_INTEGRATION_TEST_IMAGE} \
  --omni.endpoint https://localhost:8099 \
  --omni.talos-version=${TALOS_VERSION} \
  --test.run "TestIntegration/Suites/(ScaleUpAndDownAutoProvisionMachineSets)" \
  --omni.infra-provider=proxmox \
  --omni.scale-timeout 20m \
  --omni.provider-data="{${PROVIDER_DATA}}" \
  --test.failfast \
  --omni.kubernetes-version=${K8S_VERSION} \
  --test.v
