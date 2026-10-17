# Talos images and initial extensions

Choose a manually prepared Talos OpenStack disk image or opt in to shared Factory
imports. Images must be UEFI-compatible and available in the selected AHV cluster.
Import does not upgrade existing guests or automatically delete cached images.

## Automatic import

Provider runtime:

```dotenv
IMAGE_IMPORT_ENABLED=true
IMAGE_FACTORY_ENDPOINT=https://factory.talos.dev
```

MachineClass provider data, alongside sizing/network settings:

```yaml
cluster_name: example-cluster
container_name: example-storage
image_import: true
initial_extensions:
  - siderolabs/util-linux-tools
  - siderolabs/qemu-guest-agent
```

Both opt-in gates are required. Clear `image_name` and `image_uuid` in the class.
Omitted/empty manual fields are accepted with automatic import; nonempty ones conflict.
Without import, select a nonempty manual name/UUID matching the desired Talos release
and extensions. A nonempty initial-extension list cannot customize a manual image.

Automatic import uses **amd64 OpenStack QCOW2**. Imports target only the VM's selected
AHV cluster; there is no separate destination list, all-clusters switch or prewarming.
The storage container is separately required for VM disk placement. Existing Prism
image-placement policies may distribute extra copies; the provider does not change them.

## Initial Talos extensions

The MachineClass form exposes **Initial Talos extensions** (`initial_extensions`):

- Use exact official names such as `siderolabs/util-linux-tools`, with no version,
  tag or digest. Factory resolves availability for the requested Talos release.
- Names match `siderolabs/[a-z0-9][a-z0-9-]*`, at most 128 ASCII bytes each. Custom
  registries, credentials, whitespace, non-string values and tagged names are rejected.
- The list adds to `MachineRequest.extensions` supplied by Omni. Both input lists
  are bounded to 256 entries; the sorted/deduplicated union must have at most 256 names.
- The copied allocation provider data, not a live MachineClass lookup, determines the
  new image plan. Equivalent unions share a cache regardless of the source split.
- Editing the class affects **new allocations only**. Existing frozen plans/VMs
  are unchanged; later extension management belongs to Omni.

This is a name list, not a release-specific extension catalog. Confirm availability
and compatibility with the requested Talos release/hardware. With authorized
Talosconfig, `talosctl -n <node> get extensions` reads installed guest extensions.
Do not share the Talosconfig or full guest configuration.

### Changing extensions on existing machines

Use Omni's machine **Extensions** tab or native desired `systemExtensions` settings.
Installation changes use Talos image upgrades and can restart nodes; follow your
maintenance procedure. Class-only initial entries do not modify Omni's desired-
extension resources, so explicitly include required extensions in later upgrade
settings rather than assuming the class list controls every future image.

## Request customization

Before freezing a new plan, the provider reads the owned Running Omni request's explicit
`KernelArgs`, `MetaValues`, `Extensions` and `Overlay`, plus copied initial extensions.
Connection parameters, join configuration, machine UUID and unique tokens stay in the
per-machine config drive, **not** in the shared schematic.

- Kernel argument order/repetitions are retained. Each entry is a nonempty UTF-8
  token, at most 4096 bytes, without whitespace/control characters. Connection,
  identity/platform and lifecycle overrides such as `siderolink`, `talos.config`,
  `talos.meta`, `talos.platform`, `talos.events`, `talos.logging` and
  `talos.experimental.wipe` (including dotted subkeys/removal forms) are rejected.
- META keys must be unique non-null integers 0–255; values must be UTF-8 without NULs.
  Reserved upgrade/staged-install, encryption, download-code, UUID/token and bootloader
  keys are rejected. User-reserved keys 12–14 can contain non-secret shared metadata.
- Overlays accept credential-free name/image only, using a fully qualified OCI image
  with explicit tag or SHA256 digest. Options, bootloader/Secure Boot changes and
  authenticated references are unsupported. Confirm amd64 OpenStack compatibility;
  prefer a trusted digest.
- Each list is limited to 256 entries and canonical schematic YAML to **64 KiB**.
  The locally computed schematic identity must match Factory's response.

Do not put secrets in customization: it is uploaded to Factory and embedded in a
shared artifact. Kernel/META/overlay/extension differences isolate cache identities.
Frozen plans replay their original source; class edits do not rewrite them.

## Trust and image verification

Factory defaults to `https://factory.talos.dev`; `IMAGE_FACTORY_CA_FILE` adds public
CA trust without disabling verification. Provider and PC must trust/reach Factory
and **`https://assets.factory.talos.dev`** when public assets redirect.

The provider accepts one image GET redirect from the public canonical Factory to that
exact HTTPS asset host. Extra hops, custom-factory redirects, nonstandard ports,
userinfo, fragments, HTTP downgrades and schematic-POST redirects are refused.
Signed asset queries are used only transiently and never logged, persisted or sent
to Prism. Redirected requests carry no credentials/cookies/referrer.

Downloads are bounded to **8 GiB / 10 minutes**. QCOW2 is streamed without saving it
to disk and SHA256 is computed. Prism image creation uses GA
`POST /api/vmm/v4.0/content/images`, verified URL fetching, SHA256 and frozen cluster
locations. Provider CA configuration does not configure PC's trust. Checksums provide
integrity, not an independent artifact signature.

Before VM creation the provider requires a successful task and exact image UUID/name,
ownership marker, canonical source URL, checksum, DISK_IMAGE type, positive size and
every frozen location. Name discovery identifies a single candidate by LIST, then
verifies a fresh GET. Missing/ambiguous/unauthorized metadata blocks reuse.

## Shared caches and retained state

Source/release/schematic and selected cluster determine a provider-scoped shared cache.
Requests with the same identity reuse its verified image. A different cluster or image
customization gets a separate cache, not edits to the old one.

Durable Omni `ImageImport` journals retain frozen plans, checksum, atomic submission
claim, task and verified image UUID independently of VM teardown. Claims precede image
POST. A lost result is not permission to submit a fresh attempt. Existing submitted
plans replay their original identities and remain compatible with existing state.

Keep **one active provider/Recreate** and retain every image journal, replacement
marker and recovery approval. Teardown does not cancel/delete a shared import; it can
finish after the initiating allocation disappears. The provider does not garbage-
collect images or retained state.

### Confirmed deleted completed-image replacement

When an operator deliberately deletes a previously verified completed Prism image,
the next matching **new Omni allocation** can select one shared successor:

1. The submitted Running parent journal records a verified image UUID. Its exact GET
   must return 404 and exact-name discovery must succeed with no candidate. Permission
   errors or RBAC-hidden images are not deletion evidence; review
   [scope visibility](prism-permissions.md#scope-and-visibility) first.
2. An immutable `ImageImportReplacement` marker freezes the parent identity/hash/
   metadata. A deterministic successor journal precedes marker publication and uses
   the same source/checksum/destinations with fresh shared submission identities.
3. New allocations freeze the lineage. Canonical QCOW2 is rehashed and deletion/
   parent/marker integrity rechecked around the shared claim. Already frozen requests
   and VMs never retarget.
4. Failed/unknown tasks, lost responses and post-claim drift never reset/rotate an
   attempt. Missing successor journals are not recreated. Repeated confirmed deletion
   can form a chain bounded to **16** replacements; never prune it to retry.

The provider does not delete images. A frozen pre-VM request whose image disappears
stays blocked rather than acquiring a different image silently.

### Explicit retained-import recovery

`provider image-import-recovery` is an administrative review/activation command,
not automatic repair of arbitrary uncertain imports. Without `--apply` it is GET-only
and neither starts the provider nor refreshes registration/imports an image.

Review requires the exact original cache/spec hash/COSI version/creation time, retired
image UUID and successful image-task family, an explicitly reviewed fresh random v4
attempt UUID and zero provider requests/machines for initial activation. Supply 1–16
exact task IDs including every reported parent/root/subtask. Each must identify the
retired image/name and all frozen destinations with valid operation times; unrelated,
ambiguous or incomplete evidence blocks. Exact retired-image 404 and successful
exact-name absence are mandatory. Never invent a new attempt UUID to retry an approval.

Illustrative GET-only review; replace every placeholder from actual reviewed evidence:

```sh
provider image-import-recovery \
  --cache-id '<reviewed-original-cache-id>' \
  --expected-journal-sha256 '<reviewed-spec-sha256>' \
  --expected-journal-version '<reviewed-cosi-version>' \
  --expected-journal-created '<reviewed-UTC-RFC3339Nano-timestamp>' \
  --retired-image-uuid '<reviewed-deleted-image-uuid>' \
  --task-id '<exact-task-id>' \
  --attempt-uuid '<fresh-reviewed-v4-uuid>'
```

Only after review, identical inputs with `--apply` create an unsubmitted successor and
immutable `ImageImportRecovery` approval in Omni; activation calls neither Factory nor
Prism POST. An interrupted exact unsubmitted seed may finish after fresh review. An
already activated identical approval returns a receipt only—do not repeat activation
after a session/observation interruption.

Later user allocations can trigger the frozen successor import after fresh source,
parent/approval, task-family and absence checks around its claim. Unknown submissions
complete only by exact full image verification. Erased parents/approvals/successors
are not reconstructed. One explicit successor is permitted per original cache; keep
its approval intact through later normal confirmed-deletion replacement.
