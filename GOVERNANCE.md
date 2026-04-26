# Governance

`mato` currently uses a single-maintainer governance model. The project owner and lead maintainer is [@ryansimmen](https://github.com/ryansimmen).

## Decision Model

Project decisions are made in public issues, pull requests, and repository discussions where practical. The maintainer seeks feedback for substantial user-visible behavior, security-sensitive changes, release-process changes, and major design changes.

The maintainer makes the final decision when consensus is unclear, when a change must be scoped down, or when a tradeoff is needed between compatibility, security, maintenance cost, and project direction.

## Roles And Responsibilities

The maintainer is responsible for:

- triaging issues and pull requests
- reviewing proposed changes for correctness, safety, maintainability, and project fit
- maintaining the roadmap and documentation
- running or verifying release checks
- publishing releases and release notes
- responding to security reports under [SECURITY.md](SECURITY.md)
- enforcing the [Code of Conduct](CODE_OF_CONDUCT.md)

Contributors are responsible for:

- keeping changes focused and reviewable
- adding or updating tests for behavior changes
- updating documentation for user-visible changes
- explaining validation performed before requesting review
- following the Code of Conduct

## Review Expectations

Pull requests should describe the problem, the intended behavior change, and the validation performed. Changes that affect runtime behavior, task format, security boundaries, installation, or releases should include tests or a clear explanation for why tests are not practical.

The maintainer reviews pull requests before merge. The review checks for correctness, regressions, security impact, documentation impact, test coverage, and consistency with the project roadmap. Maintainer-authored changes may be merged by the maintainer after CI passes; external contributions are reviewed before merge.

## Disputes And Scope

Disagreements should start with technical discussion in the relevant issue or pull request. If discussion does not converge, the maintainer makes the final call and should explain the decision briefly.

The project may decline changes that are correct in isolation but increase maintenance cost, weaken the safety model, conflict with documented non-goals, or broaden platform support beyond the current scope.

## Continuity

The project is maintained from the public GitHub repository at <https://github.com/ryansimmen/mato>. Public issues, pull requests, releases, documentation, and source history are the durable project record.

The current public maintainer bus factor is one. The maintainer keeps private account-recovery and release-continuity arrangements outside this repository, but no second public maintainer currently has standing authority to create releases or administer the repository. Private recovery material, credentials, tokens, and personal emergency details are not stored in the public repo.

If the maintainer becomes unavailable, contributors should use the public repository record to continue development by opening issues, preparing pull requests, or forking the project as needed until maintainership can be restored or transferred. Adding a second trusted maintainer with documented release authority is a project goal before claiming full OpenSSF access-continuity coverage.
