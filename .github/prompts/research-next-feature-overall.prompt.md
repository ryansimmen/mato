---
description: "Research the best next mato features when no feature area is known yet"
name: "research-next-feature-overall"
agent: "feature-research-debate"
tools: [read, search, web, agent]
argument-hint: "Optional constraints, priorities, or user segments to bias the debate"
---
Use `feature-research-debate` to determine what `mato` should build next when the user does not already know the feature area.

Start by inferring the main product areas, workflows, and user jobs from the repository and docs before debating feature candidates.

Repository-specific context:

- `mato` is a CLI for Multi Agent Task Orchestration.
- It manages markdown task files, filesystem queue state, agent execution, task claiming, review, merge, and status workflows.
- It should favor features that improve the core orchestration product rather than speculative platform expansion.

Objectives:

- Infer the most important product areas from the current repository shape.
- Identify the best new feature opportunities across the repo overall.
- Determine what should be built next, and what should explicitly not be built yet.

Default constraints:

- Favor high user value for developers using `mato` for queued task execution and agent workflows.
- Favor features that fit the current architecture and conventions.
- Prefer small surface area, low maintenance cost, and realistic implementation scope.
- Avoid broad platform expansion, speculative integrations, and large refactors unless the repo strongly suggests they are necessary.
- Treat “build nothing in the next cycle” as a valid conclusion if the evidence supports it.

Required output additions beyond the agent's normal format:

- Start with a short `Inferred Product Areas` section summarizing the main workflows and opportunity areas you found in the repo.
- In `Debate Highlights`, explicitly include what not to build yet and why.
- In `Validation Gaps`, call out the single biggest uncertainty that most affects prioritization.

If the user provides extra constraints after invoking this prompt, treat them as higher priority than the defaults above.

Optional user-supplied context:

${input:constraints:Optional constraints, priorities, non-goals, or target users}
