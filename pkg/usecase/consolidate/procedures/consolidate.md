<!-- Original text: MIT (LICENSE). Research ideas: ACE, Reflexion, ReasoningBank and Letta. Sources retain their own licenses. See THIRD_PARTY_NOTICES.md. -->
# Consolidate memory

Read recent episodes with `episodes_since`. Omit `since` to use the previous successful run.
Treat correction keywords, reactions, repeated calls, and tool errors as clues to inspect, not proof.
Treat ambiguous and context-only episodes as context rather than evidence of a specific turn.

## Learn from the episodes

Record facts the owner taught and clear factual corrections.
When several episodes show the same lesson, write it once as a general rule in the topic or procedure where it applies. Do not infer a rule from one failed conversation.

## Tidy the memory you maintain

Everything outside `owner/` is your own memory. Keep it short, current, and easy to find:

- Delete what no longer matters: finished todo items, logs of past reports, superseded facts.
- Replace a running log with its latest state. A todo item keeps its request, link, done condition, and current status, not a history of every check.
- Merge duplicates. Generalize several specific notes into one rule when they say the same thing.
- Move a fact to the file where a reader would look for it. Split a file that mixes topics. Keep `core.md` to what every turn needs.

Make each change a separate commit.
Do not rewrite a whole document in one edit: whole rewrites lose detail.

## Propose the rest

Send changes to `owner/`, to tools, and to the harness to the owner as proposals. Do not implement them here.
For a requested behavior experiment, read `procedures/hillclimb.md` in a separate owner-requested turn.

If you have proposals that you have not sent before, call `reply` with `target: "owner"` and send only those. Record each sent proposal in memory as an open item, and keep it until the owner decides or it no longer applies. Do not send an open proposal again. With no new proposal, post nothing. Then end the turn.
