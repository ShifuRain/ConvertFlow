# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately**. Do not open a public issue or pull request.

Use GitHub private vulnerability reporting: open the repository's **Security** tab, choose **Report a vulnerability**, and describe the problem. Helpful details: affected version or commit, component (api, worker, UI, compose/hardening, build pipeline), steps to reproduce or a proof-of-concept (for media-parsing issues: the smallest file that triggers it, if you may share it), and the impact you expect.

Repository owner: private vulnerability reporting must be enabled under Settings, Code security and analysis, Private vulnerability reporting. Until it is enabled, reporters cannot use the flow above. This file does not name a mailbox because none has been decided for the project.

## What to expect

This is a small project run on a best-effort basis; there is no formal SLA. The maintainers aim to acknowledge a report within about 7 days, to confirm or dispute it, to agree a disclosure date with you, and to credit you in the release notes if you wish. Fixes are released as new versions; there is no backport policy yet beyond the latest release.

## Scope

In scope: the ConvertFlow code in this repository, its container images and compose file, and the build/release pipeline.

Out of scope or known and documented:

- The **absence of authentication** is a design decision, not a vulnerability ([docs/security.md](docs/security.md), [docs/risk-acceptance-template.md](docs/risk-acceptance-template.md)).
- Vulnerabilities in FFmpeg, codec libraries, PostgreSQL or base images should also be reported to those upstream projects; reports here are still welcome when they concern how ConvertFlow uses or pins them.
- Findings that need access to the host, the container runtime or the database already.

## Supported versions

Only the latest release receives security fixes. There has not been a release yet.
