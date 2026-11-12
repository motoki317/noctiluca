<!-- Original text: MIT (LICENSE). Research ideas: ACE, Reflexion, ReasoningBank, Letta, and Mem0. Sources retain their own licenses. See THIRD_PARTY_NOTICES.md. -->
# Consolidate memory

Consolidation restructures memory; it does not pile up entries.

## Read

Read recent episodes with `episodes_since`, and `short-term.md` in full.
Treat correction keywords, reactions, repeated calls, edits, deletions, and tool errors as clues to inspect, not proof.

## Give each item one home

Take each lesson from the episodes and each short-term entry:

- Open work stays in short-term memory: its request, the ref to post the result at, its done condition, and its latest status, not a history of checks.
- A rule the owner taught, or one that several episodes or days confirm, goes to long-term memory, stated generally.
- A specific fact or procedure goes to external memory under its theme, including facts the owner taught or corrected.
- Delete everything else: finished work, logs of past reports, superseded facts.

Read the destination file or page in full first. Then make one of four edits: add the item, update the entry that covers it, delete the entries it makes obsolete, or change nothing.

## Restructure

- Organize external memory by theme, so that anyone can find a fact by its topic. Give a theme with several pages a hub page that links them. Merge duplicates. When a page someone else wrote obviously duplicates one of yours, merge the two into one page. Propose any other change to their pages to the owner.
- Keep long-term memory general: merge rules that say the same thing, move a rule that only one procedure needs into that procedure, and delete rules that no longer hold.
- Measure files with `wc -c`. Split a long-term or external file over 8 KB by theme. Shorten `core.md` or `short-term.md` over 8 KB by moving detail to where it belongs.
- Make each change a separate commit. Do not rewrite a whole document in one edit: whole rewrites lose detail.

## Check

Read the diff of the commits you made in this run and the external pages you changed. Every deleted open item must be done or moved, and every deleted fact must be obsolete or kept elsewhere. Restore anything else.

## Report

Propose changes to `owner/`, to tools, and to the harness to the owner instead of making them. Do not run a behavior experiment (`procedures/hillclimb.md`) here; it needs the owner's request.

If you merged pages that other people wrote, or have proposals that you have not sent before, call `reply` with `target: "owner"` and send only those, briefly. Record each sent proposal in short-term memory until the owner decides or it no longer applies. With nothing new, post nothing. Then end the turn.
