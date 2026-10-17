# Security and private reports

This community-maintained provider creates/deletes VMs and handles bootstrap
credentials. Use dedicated scoped access, verified TLS and one active process.
See [Prism permissions](docs/prism-permissions.md) and [operations](docs/operations.md).

## Reporting a vulnerability

Contact maintainers through the repository host's private security-reporting feature
when enabled. Otherwise ask maintainers to arrange a private channel without posting
sensitive details publicly. No unconfigured security address or response SLA is promised.

Provide impact, affected build metadata, a minimal fictionalized reproduction and
sanitized fixed errors. Do not post credentials, auth headers, exploit-sensitive
customer data, kubeconfigs, TLS private keys, guest config-drive/ISO/join contents,
signed download queries, full API responses or complete Deployment/environment dumps.

If access may be exposed, notify the credential owner and arrange controlled rotation.
Deleting a file/message or adding it to `.gitignore` neither revokes credentials nor
removes them from Git history. Review intended public history and host artifacts
before publication. Preserve private evidence; do not reset journals or force cleanup.

The upstream non-affiliation/support disclaimer applies. There is no guaranteed
vendor support, security response time or long-term maintenance contract. A pinned
image digest is not a provenance/signature attestation.
