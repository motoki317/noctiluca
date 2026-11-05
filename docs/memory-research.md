# Research and tools for memory consolidation

This document lists what a survey on 2026-11-04 found, and what noctiluca adopts. The decisions appear in the "Memory" and "Memory consolidation" sections of [design.md](design.md).

## Adopted

| Source | Content | Use in noctiluca |
|---|---|---|
| [Letta MemFS](https://docs.letta.com/letta-agent/concepts/memfs) | Memory is a git repository of markdown, with a commit for each edit. Temporary worktrees support memory history operations. | The shape of the memory repository itself. noctiluca uses only the idea and does not depend on Letta. |
| [Letta sleep-time agents](https://arxiv.org/abs/2504.13171) | While the main agent is idle, a separate agent rewrites the shared memory. | consolidate runs in the background. |
| [ACE](https://arxiv.org/abs/2510.04618) | It fixes the memory (a playbook) with small diffs, one item at a time. A full rewrite shrinks the content and lowers accuracy (context collapse). Optimization tends toward short, generic sentences (brevity bias). | Factual corrections change one item. Behavioral changes become proposals for the owner. No change rewrites a whole document. |
| [Reflexion](https://arxiv.org/abs/2303.11366), [ReasoningBank](https://arxiv.org/abs/2509.25140) | They extract reusable strategies in words from both successful and failed trajectories. | The failure signals (corrections, tool errors, a "cannot" that contradicts a capability) are the input. |
| [Voyager](https://arxiv.org/abs/2305.16291) | It keeps verified procedures in a library and retrieves them when needed. | Procedures live in memory files and native model skills. |
| [GEPA](https://arxiv.org/abs/2507.19457) | It compares candidates on individual cases as well as their averages. | The owner-requested hillclimbing procedure rejects changes that break previously passing cases. |
| [Automating eval design and hillclimbing with Claude](https://claude.dev/blog/automating-eval-design-and-hillclimbing/) | Separate training and held-out cases, measure noise, and revert changes without a held-out gain. | The on-demand hillclimbing procedure uses model tools. The harness has no experiment runner. |
| [Claude Code memory](https://code.claude.com/docs/en/memory) | It reads the `MEMORY.md` index (the first 200 lines or 25 KB) every time, and reads topic documents when needed. | The structure of a core plus documents linked from it. |

## Rejected

- [Mem0](https://github.com/mem0ai/mem0), [Graphiti](https://github.com/getzep/graphiti), [cognee](https://github.com/topoteretes/cognee), and [Letta](https://github.com/letta-ai/letta) itself: each needs a Python service or an external database (a vector database, Neo4j). They add dependencies for a job that markdown and git cover.
- [Basic Memory](https://github.com/basicmachines-co/basic-memory): its structure is close, but it is written in Python and licensed under AGPL-3.0.
- The importance score of [Generative Agents](https://arxiv.org/abs/2304.03442) and the automatic links of [A-MEM](https://arxiv.org/abs/2502.12110): the ideas are close, but both are research code, and the current scale (a few dozen documents) does not need them.

## What happened in OpenClaw memory-core (dreaming)

In the real data under `memory/dreaming/`, all 29 deep runs reported "Promoted 0 candidate(s)". 28 of the candidates had `recalls: 0`. Dreaming promotes memories that searches return often. As a result, knowledge that the agent forgot and never searched for never becomes a candidate. That is the opposite of the goal, which is to recover forgotten knowledge such as the user token. Dreaming also ignores user corrections and tool errors, and it does not measure whether behavior improved after consolidation.

## Search

- While there are a few dozen documents, the agent greps them itself. noctiluca builds no search index.
- When grep is not enough, one SQLite database ([ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3), no cgo, FTS5 included) holds an FTS5 trigram index and embeddings. The embeddings come from OpenAI `text-embedding-3-small`, which the owner already pays for, and are stored as BLOBs. Go computes the cosine similarity. For a few thousand items, this takes a few milliseconds.
- A trigram index does not match a word shorter than three characters. For such a word, for example a two-character Japanese word, the search uses `LIKE` or the embeddings.
