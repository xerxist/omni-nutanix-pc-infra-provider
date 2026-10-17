# Prism-backed provider health

The active provider reports bounded GA discovery results through Omni's existing
provider health mechanism. A failed check writes a sanitized error; a later successful
check clears it and updates the heartbeat. This is read-only reporting, not a
provisioning/teardown gate.

## Probes and bounds

The default interval is **30 seconds**, with a **15-second overall deadline** and
at most **64 KiB** per response. Each probe requests a single entity using GET with
`$limit=1` and `$page=0`:

| Resource | Default route |
| --- | --- |
| Clusters | `/api/clustermgmt/v4.0/config/clusters` |
| Storage containers | `/api/clustermgmt/v4.0/config/storage-containers` |
| Subnets | `/api/networking/v4.0/config/subnets` |
| Images | `/api/vmm/v4.0/content/images` |
| VMs | `/api/vmm/v4.0/ahv/config/vms` |

Checks use configured namespace selectors, Prism credentials, TLS roots and request
bounds. Empty valid arrays are accepted; malformed/oversized/unauthorized responses
fail. Storage-container identities accept GA `containerExtId` or `extId`; other probes
require `extId`. Errors omit raw bodies, credentials, URLs and underlying connection
details. A successful empty inventory does not prove objects outside RBAC scope absent.

## What it does not prove

Health does not establish VM/image write permissions, task access, storage capacity,
GPU availability, Flow enforcement, guest readiness or final VM placement. UUID-only
placement still needs these five LIST-read permissions. See [Prism permissions](prism-permissions.md).

There is no public HTTP health server or Kubernetes liveness/readiness probe. Pod
availability or a heartbeat timestamp alone is not proof of successful Prism checks;
inspect the current error/status as well. Registration-only mode remains heartbeat-
only and constructs no Prism client, so it performs no provisioning or teardown.
