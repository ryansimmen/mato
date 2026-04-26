# Security Policy

## Reporting A Vulnerability

Please report suspected vulnerabilities privately using:

- GitHub's [private vulnerability reporting](https://github.com/ryansimmen/mato/security/advisories/new)

Do not open a public GitHub issue for security-sensitive reports.

Include as much of the following as you can:

- affected version or commit
- environment details
- reproduction steps or proof of concept
- impact assessment
- any suggested mitigation

## Response Process

The maintainer will acknowledge vulnerability reports within 14 calendar days. If the report is in scope, the maintainer will validate the issue, assess impact, identify affected releases, and coordinate a fix and disclosure plan with the reporter.

For public vulnerabilities, the project target is to publish a fix or documented mitigation within 60 days. Critical vulnerabilities are prioritized for faster response where practical. Release notes identify fixed publicly-known vulnerabilities that already have a CVE or similar identifier when the release is created.

Reporters are credited in the advisory or release notes unless they request anonymity.

## Supported Versions

Security fixes are best-effort while the project is pre-`v1`. The supported lines are:

| Version | Support |
|---------|---------|
| `main` | Security fixes are developed against the latest commit. |
| Latest tagged release | Security fixes are released through a new tag when a release is needed. |
| Older tagged releases | Upgrade to the latest release unless a separate advisory says otherwise. |

Upgrade instructions are documented in [Install](docs/install.md).

## Scope Notes

`mato` is an operator tool that launches autonomous agents in Docker and may forward local GitHub authentication into containers. Reports about unsafe defaults, credential handling, command execution boundaries, container isolation expectations, or repository integrity protections are in scope.
