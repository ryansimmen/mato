# Security Policy

## Reporting A Vulnerability

Please report suspected vulnerabilities privately using one of:

- GitHub's [private vulnerability reporting](https://github.com/ryansimmen/mato/security/advisories/new) (preferred)
- Email to `ryan.simmen@gmail.com`

Do not open a public GitHub issue for security-sensitive reports.

Include as much of the following as you can:

- affected version or commit
- environment details
- reproduction steps or proof of concept
- impact assessment
- any suggested mitigation

I will acknowledge receipt and work with you on validation, impact, and a disclosure plan.

## Supported Versions

Security fixes are best-effort while the project is pre-`v1`. In general, the focus will be on the latest commit on `main` and the most recent tagged release once tagged releases exist.

## Scope Notes

`mato` is an operator tool that launches autonomous agents in Docker and may forward local GitHub authentication into containers. Reports about unsafe defaults, credential handling, command execution boundaries, container isolation expectations, or repository integrity protections are in scope.
