# Rules for working on noctiluca

- Minimalism comes first. Not carrying OpenClaw's unneeded features is the main reason this project exists. Before you add a feature, an option, or an abstraction, check that a current workload in `docs/design.md` needs it. If none does, leave it out.
- One instance runs one agent (one soul). To run several agents, the owner starts several instances, and they talk to each other on the messaging platform. Do not add multi-agent support to the core.
- Keep platform names (Slack, Discord) out of the core and the model-adapter interface. Keep model and provider names (Claude, Codex) out of the core and the messaging-adapter interface.
- An agent never waits by polling or by `sleep`. A task that must wait hands control back to the core (`watch`, `wake_at`) and ends its turn.
- Follow the layout and conventions of [traPtitech/NeoShowcase](https://github.com/traPtitech/NeoShowcase).
- Write everything in the repository (code, comments, docs, commit messages) in English. Tracked files must be publishable: no personal names, internal IDs, or private history. Plans and working notes go in `docs/plans/`, which git ignores.
- noctiluca is MIT-licensed. When you adapt code, a protocol, or a design from another project, credit it: a short header comment in the adapted file (source and license) and an entry in THIRD_PARTY_NOTICES.md. Copy third-party code only under a compatible license, with its copyright notice.
