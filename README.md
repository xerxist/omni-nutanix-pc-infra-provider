# Omni Infrastructure Provider for Nutanix Prism Central

> :warning: This is **not** an official Sidero Labs provider. It is an
> independent, community-maintained project and is not affiliated with,
> endorsed by, or supported by Sidero Labs. :warning:

Provision Talos VMs on Nutanix AHV through Omni using **Prism Central GA v4
HTTP/JSON APIs**. The provider creates and enrolls requested machines, places their
storage before power-on, and deletes owned VMs when their allocations are removed
through Omni. It does not use Prism Element APIs or adopt existing VMs.

## Features

- Cluster-scoped placement by name or UUID, with a required destination container.
- Automatic boot/data/config-drive disk migration before first power-on.
- UEFI VM creation with Secure Boot disabled and immutable creation intent.
- Manual Talos OpenStack disk images or opt-in Image Factory QCOW2 imports.
- Shared image caching with checksum/ownership/location verification and guarded
  replacement of confirmed deleted completed images.
- **Initial Talos extensions**, additive to the extensions requested by Omni.
- Explicit Omni kernel arguments, META values and image-overlay customization.
- VLAN and overlay networking, optional VPC selectors and multiple NICs.
- Existing Prism categories, optional GPU profiles, extra disks and VM descriptions.
- Prism IAM API-key or Basic authentication, mounted secrets and verified TLS.
- Read-only Prism-backed health reporting through Omni.
- Non-root Docker Compose/Kubernetes deployment and opt-in Gitea container builds.

Run **one active provider instance** with ID **`nutanix-pc`**. Kubernetes deployments
must use **one replica and Recreate**; do not run Compose and Kubernetes concurrently
for the same provider. Guest upgrades and ongoing extension management remain Omni's
responsibility. Networks, policies, storage containers and shared images are not
created/deleted as part of normal VM teardown.

## Documentation

| Guide | Contents |
| --- | --- |
| [Quick start](docs/quickstart.md) | Prerequisites, credentials, deployment and first allocation. |
| [Configuration](docs/configuration.md) | Runtime settings, secret files, TLS and source builds. |
| [MachineClasses](docs/machineclasses.md) | Placement, sizing, networking, categories, disks and GPUs. |
| [Images and extensions](docs/image-import.md) | Automatic import, initial extensions, caches and recovery. |
| [Prism permissions](docs/prism-permissions.md) | API-key/Basic setup and least-privilege operation/scope requirements. |
| [Operations](docs/operations.md) | Provider updates, teardown, credential rotation and troubleshooting. |
| [Provider health](docs/provider-health.md) | Reported signal, endpoint coverage and limits. |
| [Gitea builds](docs/container-ci.md) | Runner, registry variables, credentials and publishing. |

Configuration examples are in [`deploy/examples/`](deploy/examples/). Docker Compose
uses [`.env.example`](.env.example); Kubernetes uses templates under
[`deploy/kubernetes/`](deploy/kubernetes/). Replace fictional names/endpoints privately
and keep credentials out of Git.

## Scope

Use Prism Central/AOS/AHV combinations that expose the required GA API operations and
are supported by Nutanix's compatibility matrix. Configuring an API revision alone
does not establish compatibility, capacity or networking eligibility.

Existing-VM adoption, static/PXE pools, resizing, policy management, Secure Boot and
vTPM are outside this provider's scope. Additional disks are ephemeral and are deleted
with their VM. Keep retained import journals, replacement markers and recovery
approvals intact; uncertain operations must not be bypassed by erasing state.

This project is an independent rewrite of
[`ktijssen/omni-nutanix-infra-provider`](https://github.com/ktijssen/omni-nutanix-infra-provider).
The Go module uses the
[public repository](https://github.com/xerxist/omni-nutanix-pc-infra-provider); container builds
remain configurable through Gitea. See
[CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

License: [Mozilla Public License 2.0](LICENSE).
