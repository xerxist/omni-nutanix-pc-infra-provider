# Contributing

This is an independent, community-maintained Prism Central GA provider. Preserve the
upstream disclaimer, [MPL 2.0 license](LICENSE) and source/contributor attribution.
Use focused changes, fictional examples and accurate feature/setup documentation.

## Building

Use a Go toolchain satisfying `go.mod` and Make:

```sh
make build
```

The local binary is written to ignored `_out/`; generated protobuf Go is checked in.
See [configuration/source builds](docs/configuration.md#build-from-source) and
[container releases](docs/container-ci.md). Runtime credentials are not build arguments.
Container publication and deployment are separate from source changes.

## Implementation contracts

Preserve provider ownership, BIOS/request identity, frozen submitted payloads,
storage/migration guards, task references and durable image/replacement/recovery
state. Do not add Prism Element fallbacks, adopt existing VMs, clear finalizers or
resubmit unknown operations with fresh identities. Provider ID, protobuf field
numbers and resource identities are compatibility contracts, not release labels.

Keep strict TLS, auth isolation, bounded requests and sanitized errors. Retain one
active provider/Recreate unless a distributed admission contract is explicitly
designed. MachineClass changes apply to new plans, not edits to running VMs.

Do not include credentials, private deployment identifiers, join data or raw API
responses in source/issues. See [SECURITY.md](SECURITY.md) for private reports.
