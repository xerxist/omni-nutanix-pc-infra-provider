# Gitea container builds

The workflow [`.gitea/workflows/container.yml`](../.gitea/workflows/container.yml)
compiles source and optionally builds/publishes to **Gitea's container registry**.
Registry host/path derive from Gitea context unless explicitly overridden. It never
deploys, connects to Prism/Omni or allocates machines.

## Workflow

- Main/tag pushes, pull requests and manual runs compile source with Go selected from
  `go.mod`.
- Publishing requires successful compilation, `CONTAINER_PUBLISH_ENABLED=true`,
  a trusted push/manual event and either `main` or a canonical release tag matching
  the repository's `VERSION` file. Pull requests cannot publish.
- The image is built with reviewed digest-pinned builder/runtime images for a single
  native platform. Image identity, non-root user, entrypoint and platform are verified.
- `provider --help` and `provider --version` verify executable packaging without
  network, runtime credentials, writable root or elevated privileges. Build metadata
  must match the frozen publication plan.
- Only the publication step receives registry credentials. Isolated Docker auth uses
  password-stdin and a temporary config removed on completion/failure. Tags must still
  point to the verified image; the push reports an unambiguous registry digest.

Use **`image@sha256:<digest>`** for deployment, not mutable tags. Main builds publish
`<gitea-host>/<owner>/<image>:sha-<full-commit-sha>`; canonical `v` release tags additionally
publish the matching release-tag alias. There are no `latest`/`main` image aliases.

`VERSION` is the single release/build metadata source. Optional Git release tags must
be `v` plus that value; direct unstamped builds report `dev`. Changing source metadata
does not update an existing deployed binary. Protect main/release tags and do not move
release tags to different source. Multi-tag pushes are not atomic.

## Configure Gitea

Enable instance/repository Actions and Packages/Container Registry. Register a trusted
repository-scoped build runner. Under **Repository Settings → Actions → Variables**:

| Variable | Value / behavior |
| --- | --- |
| `BUILD_RUNNER_LABEL` | Existing worker label; defaults to `container-builder`. |
| `BUILD_IMAGE` | Required digest-pinned Go builder, satisfying `go.mod`. |
| `RUNTIME_IMAGE` | Required digest-pinned static non-root runtime with public CA roots. |
| `CONTAINER_PUBLISH_ENABLED` | Leave unset/false until publication is intended; enable with exactly `true`. |
| `REGISTRY_HOST` | Optional hostname:port, without scheme/path; defaults to Gitea server host/port. |
| `IMAGE_PATH` | Optional lowercase owner/image path under the repository owner; defaults to repository owner/name. |
| `IMAGE_PLATFORM` | `linux/amd64` by default, or `linux/arm64` on a native matching daemon. Automatic Talos image import remains amd64. |

Use a Go Alpine builder and distroless static non-root runtime with reviewed immutable
digests. Preserve UID/GID `65532:65532`, `/usr/local/bin/provider` and CA trust support.

Create a dedicated package-publishing account/token authorized under the repository
owner, using the smallest supported package read/write scope (typically `write:package`).
The Actions job token is not the package-publisher credential. Under **Actions → Secrets**:

| Secret | Meaning |
| --- | --- |
| `REGISTRY_USERNAME` | Publisher account login, not the organization name. |
| `REGISTRY_TOKEN` | Its package-write token, not an administrator token. |

Store values in approved secret tooling/Gitea UI, never Git, build arguments or logs.
Checkout uses the separate job token without persisting credentials in Git config.
No Omni/Prism credentials, TLS private keys or runner-registration tokens belong here.

Enable publication and trigger a trusted main/tag push or manual run. A variable
change alone starts no build. Review Actions output and the owner's Packages page,
retain the reported digest and deploy separately using your normal change process.

## Runner and Docker access

The job environment needs Linux, Git, Bash, Node.js for the pinned Actions, Python
3.9+ and a writable Go tool cache. Publication also needs Docker CLI/Engine, BuildKit
and Buildx supporting `--load`, `--platform`, `--provenance=false` and `--sbom=false`.
The script uses the daemon's default builder; it provisions no privileged builder.

Provide verified HTTPS access to Gitea/registry, pinned action/toolchain sources, Go
dependencies and base-image registries. Add public enterprise CAs to both job and
Docker/BuildKit trust. Never disable TLS verification or make Docker sockets
world-writable.

Use an isolated daemon per job or serialize a dedicated worker. Docker socket access
is host-level privilege; do not share a daemon hosting production or unrelated
services. Protect runners from unreviewed/fork PR code even when no publication
secrets are supplied. This workflow does not configure signing/SBOM attestations,
multi-platform publication, retention/pruning or deployment.

For a standard socket on a dedicated build host, the runner must reach its daemon and
forward it into job containers. Merge this field into the existing runner config:

```yaml
container:
  docker_host: "unix:///var/run/docker.sock"
```

With standard Gitea runners, an empty `docker_host` auto-detects; `"-"` disables
forwarding. For remote/rootless setups use the actual approved connection and verified
TLS instead. Do not overwrite runner configuration or expose registration credentials.

## Diagnostics

From the same job environment:

```sh
python3 scripts/container-ci.py doctor
```

This checks Docker/Buildx reachability with bounded probes, not a build/login/push.
Fixed hints cover permissions, API mismatch, TLS, contexts, DNS and timeouts; only
UID/GID/groups, configured connection flags and socket metadata are reported. Raw
replies/endpoints/configuration are suppressed. Package credentials cannot fix an
unreachable Docker daemon. Do not share full Docker/runner/environment dumps.

References: [Gitea registry](https://docs.gitea.com/usage/packages/container/),
[Actions variables](https://docs.gitea.com/usage/actions/actions-variables/),
[Actions differences](https://docs.gitea.com/usage/actions/comparison/).
