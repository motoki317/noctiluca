<p align="center"><img src="assets/logo.svg" width="160" alt="noctiluca"></p>

# noctiluca

A minimal, self-hosted agent harness. One process connects one agent to a chat platform (Slack today) and a model CLI (Claude Code today), keeps its work in SQLite and its memory in Git. An optional nightly job reviews its mistakes and fixes its memory.

```mermaid
flowchart LR
  Slack <--> MA[messaging adapter] --> Core[core + reconciler] <--> Model[model adapter] <--> CLI[claude CLI]
  Core --- DB[(SQLite inbox)]
  CLI -->|MCP| Hub[tool hub]
  Hub -->|reply| MA
  Hub --> Mem[(memory Git repo)]
```

- [Running](docs/running.md): build, configure, run, test, release
- [Architecture](docs/architecture.md): components, message flow, interfaces
- [Design decisions](docs/design.md): what was decided and why

noctiluca is licensed under the [MIT License](LICENSE). [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) credits the projects and papers it draws on.
