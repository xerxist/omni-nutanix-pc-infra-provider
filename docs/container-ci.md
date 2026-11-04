# Container images and releases

Release images are published to the **GitHub Container Registry**:

```text
ghcr.io/xerxist/omni-nutanix-pc-infra-provider
```

Each [GitHub release](https://github.com/xerxist/omni-nutanix-pc-infra-provider/releases)
lists its image digest. Deploy **`image@sha256:<digest>`**, not a mutable tag:

```text
ghcr.io/xerxist/omni-nutanix-pc-infra-provider@sha256:<digest-from-release-notes>
```

Images are multi-arch (`linux/amd64`, `linux/arm64`); the runtime selects the
matching platform. Automatic Talos image import remains amd64 regardless of the
provider's platform.

## Pull and verify

```sh
docker pull ghcr.io/xerxist/omni-nutanix-pc-infra-provider:<release-tag>
docker run --rm ghcr.io/xerxist/omni-nutanix-pc-infra-provider@sha256:<digest> --version
```

`--version` prints the release tag without its `v`, for example `provider version 2.0.1`.

Each release carries a signed SLSA build-provenance attestation. With the
[GitHub CLI](https://cli.github.com/manual/gh_attestation_verify):

```sh
gh attestation verify \
  oci://ghcr.io/xerxist/omni-nutanix-pc-infra-provider@sha256:<digest> \
  --repo xerxist/omni-nutanix-pc-infra-provider
```

Image tags are the release tag (for example `v2.0.1`) and `sha-<full-commit-sha>`.
There are no `latest`/`main` aliases. A tag lookup is a convenience; record and
deploy the digest.

## Workflow

[`.github/workflows/release.yml`](../.github/workflows/release.yml) never deploys,
connects to Prism/Omni or allocates machines.

- Pull requests and `main` pushes build both platforms and smoke-test the amd64
  image (`--help`, `--version`, non-root user) with no network or registry access.
  These builds report version `dev-<commit>`. Nothing is published.
- A pushed tag `vMAJOR.MINOR.PATCH[-prerelease]` is the release version: the
  binary reports it without the `v`, at build time, with no runtime lookup. The
  tag must point to a commit already on `main`. The job repeats the smoke test,
  then pushes the multi-arch image, attests its digest and creates a GitHub
  release with the digest and generated notes. Versions containing `-` are marked as prereleases.
- Publishing uses the job's short-lived `GITHUB_TOKEN`; no personal token or
  repository secret is required. Each job receives only the permissions it needs.
- Builder and runtime images and all actions are pinned by digest/commit SHA.

## Cut a release

1. Merge the changes to `main` and wait for its verify run to pass.
2. Tag that commit and push the tag:

   ```sh
   git tag -a v2.0.1 -m v2.0.1
   git push origin v2.0.1
   ```

3. Follow **Actions → Release**. The release page lists the digest to deploy.

Do not move or reuse release tags. To fix a bad release, publish a new version.

## One-time repository setup

- **Actions:** allow GitHub Actions for the repository. Workflow-level permissions
  are declared in the file; the repository default can stay read-only.
- **Package visibility:** the first publish creates a **private** package. To allow
  anonymous pulls, open the package under **Your profile → Packages →
  omni-nutanix-pc-infra-provider → Package settings → Change visibility** and choose
  Public. GitHub does not allow a public package to become private again
  ([docs](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility)).
  Keep it private if needed and use a read-only pull credential
  (`read:packages`) in the deployment instead.
- **Tag protection:** add a repository ruleset for tags matching `v*` that
  restricts creation, update and deletion to maintainers.

## Updating build inputs

The workflow's `BUILD_IMAGE` and `RUNTIME_IMAGE` pin reviewed multi-arch index
digests. When updating, change tag and digest together. The builder's Go version
must meet the minimum in `go.mod`; the Dockerfile keeps `GOTOOLCHAIN=local`, so
the build never downloads a different toolchain. This build toolchain does not
select or upgrade the Talos version requested by Omni.

Preserve UID/GID `65532:65532`, `/usr/local/bin/provider` and CA trust support in
any replacement runtime image.

## Dependency updates

[Renovate](https://docs.renovatebot.com/) ([`renovate.json`](../renovate.json)) opens
weekly PRs for Go modules, pinned actions and these builder/runtime digests, after a
3-day minimum release age. The Go builder image and the Dockerfile `GO_VERSION` are
grouped, as are the Omni/Talos/COSI modules. The `go` directive and `replace`
directives are left for manual review. Each PR runs the verify job; nothing merges
automatically.

## Local builds

`make image IMAGE=<name:tag>` builds a single-platform image locally without
pushing; see [build from source](configuration.md#build-from-source).
