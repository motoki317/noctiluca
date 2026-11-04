# Requirements

These requirements come from the owner's requests.

## Philosophy

The harness aims to be alive. It sleeps on a schedule to consolidate its memory, learns from the successes and failures of real tasks, and gets smarter the more it is used.

## Functional requirements

### R1. Replaceable messaging platform

- At first, noctiluca supports only a Slack chatbot.
- Later, it must support other platforms such as Discord. For this reason, the core holds no concept of any single platform such as Slack.
- A platform adapter implements a minimal interface to the core. The adapter provides platform-specific operations, such as Slack search or reactions, as tools with usage notes, in the way an MCP server does. These tools do not shape the core design.

### R2. Replaceable model

- At first, noctiluca provides an adapter that uses a Claude subscription within its terms of use, as the current claude-cli runtime does.
- Later, it provides an adapter for an OpenAI subscription, such as Codex. For this reason, the core holds no concept of any single model or provider.
- A message that arrives while a turn runs goes into a queue and reaches the turn without stopping it. This is the steer behavior of the current claude-cli runtime. The interface between the core and a model adapter must be able to express it.

### R3. Minimal, clean interfaces

The two interfaces, core to messaging adapter and core to model adapter, start minimal.

### R4. Scheduled runs

- The core has one scheduled run, for example once an hour, which is the equivalent of the OpenClaw heartbeat. In it, the user runs what they want, such as a daily photo post, or a check of the todo list with reminders.
- Separately, the user can register a task with any cron expression.

### R5. Memory consolidation (important)

The harness consolidates its memory, for example after a scheduled run. This is close to OpenClaw dreaming, but it covers more and has fewer limits.

- It reviews the recent tasks, especially the tasks of that day. Where a task went wrong, it works out how to reorganize the memory (the prompts or the memory database) so that the task succeeds.
- It remembers the task or the tool concretely, so that it can use them next time. It also generalizes that ability and learns it.
- An LLM can read only a limited amount of text at once. The harness organizes the memory with structure, links, and a memory database.
- If possible, it measures whether the task actually succeeds before and after consolidation. It also forgets or generalizes memory that it does not need, to keep these three limits:
  - It does not lose too much generalization (it does not overfit).
  - It does not let a single document grow huge.
  - It does not let the number of documents or their total size grow too much.
- To avoid overfitting, it follows [Automating eval design and hillclimbing with Claude](https://claude.dev/blog/automating-eval-design-and-hillclimbing/).

### R6. Serial background tasks

Scheduled runs, cron tasks, and memory consolidation run as background tasks. Each task takes a lock, so no two run at the same time, and they run in order.
