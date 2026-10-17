# Operations and troubleshooting

Keep one active provider process, stable provider ID and all durable Omni state.
Use the [configuration](configuration.md), [permissions](prism-permissions.md) and
[image lifecycle](image-import.md) guides for setup details.

## Monitoring

Monitor the running image digest, process availability/restarts, fresh Omni provider
health, MachineRequest stages/errors and exact Prism operation tasks. Check guarded
storage completion and guest Connected/Ready separately from provider registration.
Installed Talos/extensions and Kubernetes readiness are guest/cluster signals, not
properties of a healthy provider process.

Shared images and retained journals are not garbage-collected; plan capacity and
retention without erasing active/frozen image identities. See
[provider health](provider-health.md) for endpoint coverage and limits.

## Updating the provider

Verify the source revision, image platform and immutable registry digest. Record current
configuration and privately snapshot requests, machines and every image journal,
replacement marker and recovery approval. Prefer an idle deployment for changes.

Update only the reviewed image, retaining **one replica/Recreate**, provider ID,
credentials, TLS mounts and configuration. Verify the running digest/binary metadata,
schema, fresh health and retained state afterward. A provider update does not upgrade
Talos/Kubernetes or change existing frozen image/VM plans.

If interrupted, inspect saved intent/receipts and live state read-only before acting.
Do not blindly repeat an image patch, activate recovery again, reset journals or
automatically roll back an unknown result. Never start a second active instance for HA.

## Credentials and extensions

Credentials/settings are loaded at startup. Follow
[credential rotation](prism-permissions.md#trust-and-credential-rotation): confirm new
effective access, update private files/Secrets, restart through Recreate and verify
before retiring the old credential. Registration-only mode disables teardown too;
it is not a standby mode for active allocations.

The MachineClass initial-extension list affects new images only. Use Omni's desired
extension controls for existing guests; changes can trigger Talos upgrades/restarts.
See [extension management](image-import.md#changing-extensions-on-existing-machines).

## Normal teardown

Remove allocations **through Omni**. The provider checks exact request/BIOS/VM/cluster
ownership, waits for relevant creation/power/migration tasks and idle metadata, and
deletes only the owned VM. A partial failed creation can still require identity-aware
cleanup. Finalization waits until deletion is confirmed.

Retained migration task IDs after success are deliberate; determine pending migration
from validated intent/retry/completion evidence, not task-ID presence alone. Teardown
does not submit new disk migrations or delete shared images, journals, categories,
networks, policies or storage containers. Do not erase intent, remove finalizers or
run broad VM cleanup to bypass an unknown operation.

## Troubleshooting

| Symptom | Review first |
| --- | --- |
| HTTP 401 | Credential type/expiry, conflicting sources and mounted-file readability. Never print the key/password. |
| HTTP 403 | Exact denied route, effective IAM operation/dependencies and entity scope; do not substitute Super Admin. |
| Empty inventory/404 | Scope visibility and exact entity identity. A hidden image is not proven deleted. |
| TLS/DNS/connection failure | HTTPS origin/port, public CA trust, DNS/routing and provider-network access; keep verification enabled. |
| Image import blocked | Both opt-in gates, empty manual selectors, Factory/asset trust from provider and PC, exact ownership/source/checksum/location. |
| Guest never enrolls | DHCP/DNS, Omni/SideroLink routing/Flow rules, requested image and guest public-CA trust. |
| Migration wait | Frozen intent/task/retry/completion evidence, current OFF/idle storage metadata and external operations. |
| HTTP 412/unknown submission | Frozen ETag/request identity and known task outcome; do not guess a fresh identity. |
| Missing/ambiguous GPU/category | Exact names, required catalog/definition read/use access and hardware compatibility. |
| Blocked teardown/finalizer | Terminal task state, exact partial VM identity and delete permission; do not force finalization. |

Migration admission waits can requeue before eventual provisioning. Only a narrowly
guarded terminal failed task with exclusively structured **VMM-30716** evidence and
unchanged OFF/idle source inventory permits the specialized retry; this is not a
general retry-on-failure policy. Do not clear task IDs or power on early.

## Sharing diagnostics

Provide expected/observed behavior, relevant binary/product metadata, sanitized
fixed errors/HTTP codes, request stage and a fictionalized minimal MachineClass.
Share exact task/entity identities only through an approved private channel if needed.
Never share secrets, auth headers, full Deployments/environments, kubeconfigs, private
TLS keys, guest configuration/ISO contents, signed asset URLs or raw API bodies.
See [SECURITY.md](../SECURITY.md).
