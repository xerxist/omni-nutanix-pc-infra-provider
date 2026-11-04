# Quick start

Deploy one provider for an existing Omni installation, then allocate machines through
Omni. This repository does not install Omni, Prism, Kubernetes or authentication services.

## 1. Prepare the environment

- Use Prism Central and registered AHV clusters exposing the required **GA v4.0**
  operations. Check your PC/AOS/AHV pairing in Nutanix's support matrix.
- Select an existing AHV cluster, storage container and guest subnet. Configure
  DHCP/DNS, routing, firewall and Flow rules for Omni bootstrap and cluster traffic.
- Prepare a dedicated [Prism API-key or Basic identity](prism-permissions.md).
- Ensure the provider can reach the Omni HTTPS API and Prism Central HTTPS origin.
  For Factory imports, both provider and PC need Factory/asset-host access and trust.
- Select a provider image pinned by registry digest. Release images are published to
  `ghcr.io/xerxist/omni-nutanix-pc-infra-provider` for amd64 and arm64; copy the digest
  from the [release notes](https://github.com/xerxist/omni-nutanix-pc-infra-provider/releases).
  See [container images](container-ci.md) or [a source build](configuration.md#build-from-source).

## 2. Create provider-bound Omni access

Open **Settings → Infra Providers** in Omni and create provider ID **`nutanix-pc`**.
Transfer its displayed key directly into approved secret storage. Use this provider-
bound key, not a general operator/admin key.

If using the CLI, check the installed `omnictl infraprovider create --help` first.
The equivalent command `omnictl infraprovider create nutanix-pc` **outputs a secret**;
run it only in a private terminal without session recording. Do not repeat it to
replace an existing ownership identity. See
[Omni's infrastructure-provider guide](https://docs.siderolabs.com/omni/omni-cluster-setup/setting-up-the-bare-metal-infrastructure-provider).

## 3. Prepare secrets and TLS trust

Store the Omni key and **either** Prism API key **or** paired username/password in
read-only secret files. The process runs as UID/GID **65532**; arrange directory/file
permissions so it can read them without making them world-readable.

Example runtime settings use paths, not secret values:

```dotenv
OMNI_ENDPOINT=https://omni.example.invalid
OMNI_SERVICE_ACCOUNT_KEY_FILE=/run/secrets/OMNI_SERVICE_ACCOUNT_KEY
NUTANIX_ENDPOINT=https://pc.example.invalid:9440
NUTANIX_API_KEY_FILE=/run/secrets/NUTANIX_API_KEY
PROVIDER_ID=nutanix-pc
OMNI_INSECURE_SKIP_VERIFY=false
NUTANIX_INSECURE_SKIP_VERIFY=false
```

Replace fictional endpoints privately. For Basic authentication, clear API-key inputs
from all sources and configure both username/password file paths instead. Mount public
CA bundles under `/etc/provider/certs` where enterprise trust is needed; never mount
TLS private keys, kubeconfigs or join configuration. See [all settings](configuration.md).

## 4. Choose one deployment method

Do not run multiple active processes for the same provider ID. Kubernetes must use
**one replica and Recreate**, not RollingUpdate. Migration admission is process-local.

### Docker Compose

```sh
cp .env.example .env
# Privately fill endpoints, digest-pinned PROVIDER_IMAGE, host secret/CA directories
# and container file paths. Do not put credential values in .env.
docker compose up -d
```

Set `PROVIDER_IMAGE` to the digest from the release notes, for example:

```dotenv
PROVIDER_IMAGE=ghcr.io/xerxist/omni-nutanix-pc-infra-provider@sha256:<digest>
```

Populate the private secret files matching the configured container paths. The Compose
example defaults to lowercase filenames; the Kubernetes example uses uppercase Secret
keys. Follow the paths selected in your configuration. For Basic, clear
`NUTANIX_API_KEY_FILE` and set both username/password paths.

Compose starts an already-built image; it does not implicitly build or publish one.
An empty public-CA directory is sufficient with public endpoint roots. Do not print
the complete populated `docker compose config` or environment.

### Kubernetes

```sh
cp deploy/kubernetes/deployment.env.example deployment.env
cp deploy/kubernetes/runtime.env.example runtime.env
# Privately fill both ignored copies; set PROVIDER_IMAGE to
# ghcr.io/xerxist/omni-nutanix-pc-infra-provider@sha256:<digest>.
python3 scripts/render-deployment.py --env-file deployment.env \
  --output deploy/rendered/deployment.yaml
```

The renderer requires all Deployment inputs, refuses overwrites and does not contact
Kubernetes. It creates the Deployment manifest only, not its referenced resources.

Create the selected namespace, runtime ConfigMap, credential Secret and public-CA
ConfigMap using your cluster's approved configuration/secret tooling. Secret keys must
match runtime file paths, for example `OMNI_SERVICE_ACCOUNT_KEY` and `NUTANIX_API_KEY`;
Basic uses separate username/password keys. The CA ConfigMap can be empty with public
endpoint roots. A private registry or private GHCR package needs a read-only pull Secret in a local overlay.

Review and apply the rendered Deployment. It uses one replica/Recreate, non-root
execution, read-only root/mounts and dropped capabilities. Kubernetes API access is
not needed; service-account-token automount is disabled. Keep generated manifests
and credential material out of Git.

## 5. Confirm registration

Verify one active instance and the expected image digest. In Omni, confirm `nutanix-pc`
registration, the MachineClass form and a fresh successful Prism-backed health result.
Pod Ready/heartbeat alone does not prove VM write permission or guest readiness.

Registration-only mode publishes schema/heartbeat but does not provision, tear down
or contact Prism. It is not an HA standby; do not run it beside an active instance
with the same ID.

## 6. Create a MachineClass

Use the Omni form ([illustration](machineclasses.md)) or
[the resource example](../deploy/examples/machineclass.yaml).
Minimal automatic-image provider data:

```yaml
cpu: 1
cores: 2
memory_mb: 4096
disk_gb: 40
cluster_name: example-cluster
container_name: example-storage
subnet_name: example-vlan
image_import: true
initial_extensions:
  - siderolabs/util-linux-tools
```

Enable provider-wide `IMAGE_IMPORT_ENABLED=true`, check **Automatic image import**
and clear both manual image selectors. Initial extensions are official names without
versions/tags/digests. Alternatively, disable import and select a UEFI-compatible Talos
OpenStack DISK_IMAGE matching the requested release/extensions.

Prefer UUID placement selectors. Omit categories, GPU and extra-NIC fields unless you
have configured those features. Select **Tunnel Enabled** if Omni uses HTTPS SideroLink
tunneling. See [MachineClasses](machineclasses.md) and [images](image-import.md).

## 7. Allocate through Omni

Select the MachineClass in an Omni cluster/pool and request the desired capacity.
The [cluster-template example](../deploy/examples/cluster-template.yaml) shows the
resource shape; render its name, release and count placeholders before using it.
Do not create an AHV VM manually for the provider to adopt.

Follow the MachineRequest stage/error and Prism tasks, storage completion, guest
enrollment, Omni Connected/Ready and Kubernetes Ready. Verify installed Talos/extensions
separately when required. Resolve DHCP, routing, permissions or TLS issues without
weakening guards. See [operations](operations.md).

Remove allocations **through Omni** when no longer needed. Normal teardown deletes
only the owned VM; shared images, import journals, categories, networks and containers
remain. Do not clear finalizers or durable intent to force cleanup.
