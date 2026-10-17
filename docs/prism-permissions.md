# Prism Central credentials and least privilege

Use a **dedicated non-administrator identity** for this provider. API keys and
Basic authentication both use Prism IAM authorization; the credential format does
not limit access by itself. Never grant Super Admin merely to make a 403 disappear.

This guide maps the provider's GA API operations to required capabilities. It is a
policy specification, not a ready-made or universally minimal IAM role. Exact operation
IDs, display names, dependent permissions and scope support depend on the installed
Prism release; review effective access with your IAM administrator.

## Separate provisioning administration from runtime access

A human or separately authorized bootstrap identity creates users, roles,
authorization policies and API keys. Those administration permissions **must not be
assigned to the provider's runtime credential**. The provider does not call IAM,
manage users/tokens, edit roles or expand its own scope.

Use separate policies/roles where practical:

1. **Discovery:** read only the required clusters, containers, networks, images,
   VM inventory/details and operation tasks.
2. **VM lifecycle:** create, power on/off, migrate VM disks and delete within the
   designated provisioning scope.
3. **Optional features:** image creation for automatic import; category read/use;
   VPC and GPU catalog reads only when those selectors are used.

Review all effective policies, inherited directory-group memberships and system
roles. A restrictive new policy does not cancel another broad grant.

## Required API access

`{id}` means an entity extId; `{task}` is an opaque, path-escaped task extId, not
necessarily a UUID. LIST access and detail GET access both matter. Default namespace
versions are shown; no Prism Element v2/v3 permissions are required.

| Feature | HTTP operation | Required capability and scope |
| --- | --- | --- |
| All active deployments | GET `/api/clustermgmt/v4.0/config/clusters` and `/{id}` | Read selected registered AHV clusters and their hypervisor metadata. |
| All active deployments | GET `/api/clustermgmt/v4.0/config/storage-containers` and `/{id}` | Read destination containers and cluster ownership. No container creation/update/delete. |
| All active deployments | GET `/api/networking/v4.0/config/subnets` and `/{id}` | Read allowed VLAN/overlay subnets. No network administration. |
| All active deployments | GET `/api/vmm/v4.0/content/images` and `/{id}` | Read selected manual images and every relevant retained cache image, including verification metadata. |
| All active deployments | GET `/api/vmm/v4.0/ahv/config/vms` and `/{id}` | Read provider-created/partial VMs, disks, generated CD-ROM, migration state, GPU/category inventory and ETag. |
| VM lifecycle | POST `/api/vmm/v4.0/ahv/config/vms` | Create VMs in allowed clusters using allowed image/container/subnet references and config-drive customization. Review create-time reference-use permissions. |
| VM lifecycle | POST `/api/vmm/v4.0/ahv/config/vms/{id}/$actions/power-on` | Power on scoped provisioned VMs. |
| Normal teardown | POST `/api/vmm/v4.0/ahv/config/vms/{id}/$actions/power-off` | Power off scoped owned VMs when normal teardown requires it. |
| Normal teardown | DELETE `/api/vmm/v4.0/ahv/config/vms/{id}` | Delete scoped owned/partial VMs. Do not grant unrelated VM deletion. |
| Storage placement | POST `/api/vmm/v4.0/ahv/config/vms/{id}/$actions/migrate-vm-disks` | Migrate Virtual Machine Disk, including destination-container use. This is disk migration, not cross-cluster VM migration. |
| Async operations/recovery | GET `/api/prism/v4.0/config/tasks/{task}` | Read exact provider operation tasks and required image-task family metadata. No task cancel/delete or general task administration. |
| Automatic image import only | POST `/api/vmm/v4.0/content/images` | Create disk images with URL/checksum and allowed cluster locations. Review destination reference-use permissions. |
| VPC selectors only | GET `/api/networking/v4.0/config/vpcs` and `/{id}` | Read selected VPCs. No VPC creation/update/delete. |
| Categories only | GET `/api/prism/v4.0/config/categories` and `/{id}` | Read pre-existing selected definitions and authorize their use in the VM creation payload. No category or Flow-policy creation/update/delete. |
| GPU names only | GET `/api/clustermgmt/v4.0/config/clusters/{id}/physical-gpu-profiles` and `/virtual-gpu-profiles` | Read **both** catalogs for the selected cluster; authorize GPU use in creation where required. No host/GPU profile administration. |

Select the IAM operations corresponding to these routes in your release's role
catalog. Do not copy guessed operation UUIDs from another installation. If Prism
requires an additional dependent permission, record the exact failed operation,
release and smallest additional grant in your local policy record; do not substitute
an entire administrator role.

### Permissions not requested

The provider sends no general VM PUT/update, cross-cluster VM migration, host/cluster
management, network/container CRUD, image update/delete, image placement-policy
changes, category CRUD, Flow-policy management, guest console access, snapshots,
backup/DR or IAM/key/role administration requests. Do not grant these by default.
Creation payloads can require specific reference-use/category/GPU permissions even
though there is no separate attachment POST; validate those dependencies explicitly.

**Image Delete is not needed**, even for completed-image replacement. The operator
may separately delete a reviewed image; the provider detects confirmed absence and
creates a successor. Journal/replacement/recovery resources live in Omni, not Prism
IAM, and must not be erased to work around an authorization failure.

### Health is always read-only

Active-mode health reads five lists (clusters, containers, subnets, images, VMs),
including with manual images. UUID selectors do **not** remove these list-read
requirements. Health does not check task reads, any write permission, capacity or
guest readiness. Registration-only mode loads no Prism client/credential and
performs no Prism operations, including teardown.

## Scope and visibility

Prefer a dedicated provisioning cluster or narrowly defined resource set. Allow
read/use only of the selected containers, networks, images, optional categories and
GPU catalogs. VM lifecycle scope must cover newly created VMs **and partial VMs
left by a failed creation**, not merely a static list of current VM UUIDs.

Where supported, use a pre-existing provisioning category in the creation payload
as part of the VM authorization policy. Verify creation authorization before the
new VM exists, category assignment at create time and access to failed/partial
creations. Category scoping is not a universal recipe; supported entity filters and
dependencies vary by PC version. Do not create broad wildcard policies as a shortcut.

The provider's BIOS/request/name checks are application ownership guards, **not an
IAM boundary**. The `nutanix-pc` provider ID, cache naming and descriptions do not
automatically restrict a stolen Prism credential. Restrict the actual IAM policy.

**Visibility is part of correctness.** RBAC can hide an existing object from LIST
or return 404 for an out-of-scope detail read. A healthy empty inventory does not
prove a resource is absent. All relevant owned VMs, cache images and exact tasks
must remain visible. Changing a policy or removing image visibility can resemble
deletion and invalidate replacement/recovery evidence. Resolve authorization scope
first; never reset journals or approve replacement based only on newly hidden data.

Tasks and image creation may have coarser scope controls than VMs. If a permission
cannot be narrowed in the installed release, document that limitation and isolate
the deployment. Do not claim VM-only token isolation when required task/discovery
access reaches a broader scope.

## Option A: IAM API key

1. An authorized administrator creates the custom role(s) and reviewed scoped
   authorization policy. Use a dedicated identity, for example
   `omni-nutanix-provider`; do not reuse an administrator's key.
2. Create a Prism IAM user of type **`SERVICE_ACCOUNT`**, using the installed GA
   IAM API/SDK. The documented route is POST `/api/iam/v4.0/authn/users`.
3. Create its key through POST `/api/iam/v4.0/authn/users/{id}/keys` with
   `keyType: API_KEY`. Transfer the returned key directly into approved secret
   storage; it is shown once. Never retain the complete key-creation response in
   a terminal recording, Git, ticket, chat or CI log.
4. Associate that service-account identity with the reviewed role and entity scope
   through `/api/iam/v4.0/authz/authorization-policies`. Do **not** copy the Super
   Admin/all-entities example from introductory articles.
5. Confirm effective policy and resource visibility before switching the provider.

Nutanix's cited GA documentation manages service accounts/keys and service-account
policy assignment through IAM APIs; UI coverage varies by release. A normal
local/directory user is not the same as a `SERVICE_ACCOUNT` API-key identity.
The provider authenticates with **`X-Ntnx-Api-Key`**, not a bearer-token header.

Runtime settings (paths only, never key values):

```dotenv
NUTANIX_API_KEY_FILE=/run/secrets/NUTANIX_API_KEY
NUTANIX_USERNAME_FILE=
NUTANIX_PASSWORD_FILE=
```

Remove Basic-auth values from every environment/YAML/file source. Setting only a
new key does not override conflicting credentials; the provider rejects mixed auth.

## Option B: Basic-auth automation user

1. Create a dedicated standard local or directory user for automation, separate
   from Prism's built-in `admin`. Do not confuse it with the API-key-only
   `SERVICE_ACCOUNT` user type.
2. Assign only the reviewed custom role and resource policy. Review inherited groups
   and legacy system roles; local/directory authorization behavior must be validated
   on the installed release.
3. Store username and password in separate approved secret files. Apply your
   organization's password rotation/expiry policy; avoid a human account that may
   be disabled when its owner leaves.
4. Confirm this user's non-interactive Basic-auth access is compatible with your
   authentication policy. An interactive SSO/MFA login is not a Basic-auth credential.

```dotenv
NUTANIX_API_KEY_FILE=
NUTANIX_USERNAME_FILE=/run/secrets/NUTANIX_USERNAME
NUTANIX_PASSWORD_FILE=/run/secrets/NUTANIX_PASSWORD
```

Remove API-key values from all sources. Basic authentication must use verified HTTPS.
Environment values without `_FILE` exist for compatibility, but mounted files avoid
putting credentials in process arguments and rendered manifests.

## Review effective access

Review the full effective role/policy with an IAM administrator, including inherited
grants. Confirm the required LIST/detail reads and visibility of selected resources,
owned/partial VMs, retained images and exact operation tasks. Include optional features
only when configured and record any necessary dependency/coarse-scope limitation.

A healthy provider demonstrates discovery access, not create/migrate/power/delete
permissions. Manual images need no image-create grant; automatic import does. Narrow
actual entity scopes, not just display names or provider ownership markers. Do not
remove active identity permissions while operations are pending, and never expand to
Super Admin or erase retained state to bypass a denial.

## Trust and credential rotation

Keep `NUTANIX_INSECURE_SKIP_VERIFY=false`. Add a public CA bundle with
`NUTANIX_CA_FILE` if needed; never mount TLS private keys. Limit credential files to
the non-root provider UID/GID (65532) and authorized secret administrators. The
provider needs no Kubernetes API token and no IAM-admin credential mounted beside it.

Credentials are loaded at startup, not hot-reloaded. Rotation requires a reviewed
replacement file/secret and one-instance Recreate restart; preserve provider ID,
configuration and all retained state. Validate the new credential before revoking
the old one, and avoid interrupting uncertain operations. Do not blindly switch
back/reset state after an interrupted rollout.

## References

- [Nutanix v4 API User Guide — authentication](https://www.nutanix.dev/nutanix-api-user-guide/)
- [Nutanix IAM API-key REST setup](https://www.nutanix.dev/2025/02/21/nutanix-v4-apis-using-api-key-authentication-part-2/)
- [Nutanix fine-grained Prism RBAC](https://www.nutanix.com/blog/fine-grained-rbac-with-nutanix-prism-central)
- [Nutanix GA API reference](https://developers.nutanix.com/)

References explain IAM mechanics; introductory examples sometimes use broad admin
roles or disabled TLS. Those examples are **not** this provider's security policy.
