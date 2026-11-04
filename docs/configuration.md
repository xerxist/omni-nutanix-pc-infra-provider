# Provider configuration

Configure runtime connectivity at startup and VM placement in Omni MachineClasses.
There are no built-in Omni/Prism URLs or cluster/container/subnet identifiers.
Restart the provider to reload settings or mounted secret files.

## Omni and provider identity

| Environment variable | Purpose / default |
| --- | --- |
| `OMNI_ENDPOINT` | Required Omni API HTTPS endpoint. |
| `OMNI_SERVICE_ACCOUNT_KEY_FILE` | Recommended mounted file containing the provider-bound Omni key. |
| `OMNI_SERVICE_ACCOUNT_KEY` | Alternative direct key; never set together with `_FILE`. |
| `PROVIDER_ID` | `nutanix-pc`; must match the Omni key, class and persisted ownership. |
| `PROVIDER_NAME` | Display name; defaults to `Nutanix Prism Central`. |
| `PROVIDER_DESCRIPTION` | Display description, not an individual VM description. |
| `PROVIDER_CONFIG_FILE` | Optional provider YAML; see [config.yaml.example](../deploy/examples/config.yaml.example). |
| `PROVIDER_REGISTRATION_ONLY` | `false`; when true, publishes schema/heartbeat only, with no Prism operations, provisioning or teardown. |
| `OMNI_INSECURE_SKIP_VERIFY` | `false`; keep verification enabled. |

Equivalent CLI flags are listed by `provider --help`. Explicit flags take precedence
over their environment settings. Do not supply credentials as flags; process lists
can expose them. Never change provider ID to adopt machines or reset ownership.

## Prism Central credentials and trust

| Environment variable | Purpose / default |
| --- | --- |
| `NUTANIX_ENDPOINT` | Required HTTPS **origin**, including port; no `/api` or `/PrismGateway` suffix. |
| `NUTANIX_API_KEY_FILE` | Mounted IAM API key, sent as `X-Ntnx-Api-Key`. |
| `NUTANIX_USERNAME_FILE`, `NUTANIX_PASSWORD_FILE` | Both mounted Basic credentials; use instead of an API key. |
| `NUTANIX_API_KEY`, `NUTANIX_USERNAME`, `NUTANIX_PASSWORD` | Direct alternatives to the corresponding `_FILE` inputs. |
| `NUTANIX_CA_FILE` | Optional public PEM CA bundle, added to system Prism trust. |
| `TALOS_GUEST_CA_FILE` | Optional public PEM CA bundle for Talos-to-Omni bootstrap/retained trust, separate from Prism trust. |
| `NUTANIX_INSECURE_SKIP_VERIFY` | `false`; keep verification enabled. |
| `NUTANIX_VMM_VERSION`, `NUTANIX_NETWORKING_VERSION`, `NUTANIX_CLUSTER_VERSION`, `NUTANIX_PRISM_VERSION` | GA namespace selectors; default `v4.0`. |

Nonempty environment/file inputs override YAML. A direct credential plus its `_FILE`
is an error. Unreadable/empty secret files fail closed; one trailing LF/CRLF is removed
and spaces are preserved. Choose exactly one Prism authentication mode and remove old
credentials from **all** environment/YAML/file sources when switching.

The process runs as UID/GID **65532**. It must be able to traverse the secret directory
and read mounted files without exposing them to unrelated users. Do not mount an IAM
administrator credential. See [Prism least privilege](prism-permissions.md).

The container adds `/etc/provider/certs` to Go's `SSL_CERT_DIR` while retaining
`/etc/ssl/certs`. Mount public CA PEMs there for private endpoint roots; configure
explicit Prism/guest/Factory CA paths as needed. Do not mount TLS private keys,
kubeconfigs or join configuration. Provider trust does not configure Prism's own trust
when PC downloads an image.

## Image Factory

| Environment variable | Purpose / default |
| --- | --- |
| `IMAGE_IMPORT_ENABLED` | `false`; provider-wide opt-in, also requiring class `image_import: true`. |
| `IMAGE_FACTORY_ENDPOINT` | `https://factory.talos.dev`. |
| `IMAGE_FACTORY_CA_FILE` | Optional public PEM bundle added to Factory system trust. |

Factory HTTPS verification is mandatory. Automatic import uses amd64 OpenStack QCOW2;
there is no image-destination list or background prewarming. See
[images and extensions](image-import.md).

## Build from source

Use a Go toolchain satisfying [`go.mod`](../go.mod), plus Make. Generated protobuf Go
is checked in; normal builds do not require a generator.

```sh
make build
./_out/omni-infra-provider-nutanix --help
```

`make build` creates a static local executable in ignored `_out/`. Its version comes
from `git describe` (`2.0.1` on a release tag, `2.0.1-3-gabc1234` after it, `dev`
without tags); `provider --version` reports
that metadata without credentials or network access. It does not build/push a
container or deploy the provider. The executable accepts the same runtime settings.

`make image IMAGE=<registry/owner/image:tag> BUILD_ARGS='<reviewed build arguments>'`
is an explicit local container build, not a push/deployment. Pin builder/runtime
images; releases are published by [GitHub Actions](container-ci.md). Unstamped direct
Go/Docker builds report `dev`.

The module path is
`github.com/xerxist/omni-nutanix-pc-infra-provider`, matching the public
[source repository](https://github.com/xerxist/omni-nutanix-pc-infra-provider).
Building this checkout does not download the provider itself from GitHub.
Dependency/protocol selectors remain defined in `go.mod`/`go.sum` and the source.
