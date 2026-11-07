<p align="center"><img src="assets/social-16x9.png" alt="noctiluca: a minimal agent harness that sleeps, dreams, and learns"></p>

# noctiluca

A minimal, self-hosted agent harness. One process connects one agent to one or more chat accounts (Slack today) and a model CLI (Claude Code today), keeps its work in SQLite and its memory in Git. An optional nightly job reviews its mistakes and fixes its memory.

```mermaid
flowchart LR
  P1[chat account] <--> MA1
  P2[chat account] <--> MA2
  subgraph MA[one or more]
    MA1[messaging adapter]
    MA2[messaging adapter]
  end
  MA1 & MA2 <--> Core[core + reconciler] <--> Model[model adapter] <--> CLI[model CLI]
  Core --- DB[(SQLite inbox)]
  CLI -->|MCP| Hub[tool hub]
  Hub -->|reply| Core
  CLI -->|file tools and git| Mem[(memory Git repo)]
  MA1 -.- E1[e.g. Slack]:::example
  Model -.- E2[e.g. Claude Code]:::example
  classDef example font-size:11px,fill:none,stroke-dasharray:3 3
```

- [Running](docs/running.md): build, configure, run, test, release
- [Architecture](docs/architecture.md): components, message flow, interfaces
- [Design decisions](docs/design.md): what was decided and why

noctiluca is licensed under the [MIT License](LICENSE). [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) credits the projects and papers it draws on.
