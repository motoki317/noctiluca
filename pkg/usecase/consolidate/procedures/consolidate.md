<!-- Original text: MIT (LICENSE). Research ideas: ACE, Reflexion, ReasoningBank and Letta. Sources retain their own licenses. See THIRD_PARTY_NOTICES.md. -->
# Consolidate memory

Read recent episodes with `episodes_since`. Omit `since` to use the previous successful run.
Treat correction keywords, reactions, repeated calls, and tool errors as clues to inspect, not proof.
Treat ambiguous and context-only episodes as context rather than evidence of a specific turn.

Commit facts the owner taught and clear factual corrections with `memory_write`.
Keep each edit small and cite its evidence in the commit message.
Use `memory_sizes` to check limits. Merge duplicate facts or delete obsolete facts when needed.
Do not rewrite whole documents or infer a behavioral rule from one failed conversation.

Send behavioral, structural, tool, and harness changes to the owner as proposals.
Do not implement these proposals during consolidation.
For a requested behavior experiment, read `procedures/hillclimb.md` in a separate owner-requested turn.

Call `reply` with `target: "owner"` and a short summary of factual corrections and proposals, then end the turn.
The task's reply goes to the owner's DM.
