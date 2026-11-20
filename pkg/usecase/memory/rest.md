# Save and restructure memory

The `main` session holds the context of every conversation, scheduled task, and background result.
After `rest`, a fresh `main` process knows only its system prompt and what it reads from memory.

If procedures/rest.md exists in memory, read it first. It adds this instance's own steps, and each step says where it fits.

## Save what the next process needs

Read `short-term.md` in full, and add what it lacks:

- Open work and promised follow-ups: each request, its messaging entry and native destination IDs, done condition, and latest status.
- Pending `watch` commands and `wake_at` notes, and what each is for.
- Facts learned across conversations, tasks, and background results, under their theme in external memory.

Skip what memory already holds.

## Learn general principles

Read `core.md` and the relevant long-term files before editing them.
Compress what the owner taught or corrected into general principles in `core.md`.
Keep supporting detail in `long-term/` and link to it from `core.md`.
Infer what the owner really wanted, beyond the literal correction.
If a correction exposes wording that is too narrow, broaden the existing principle to cover that intent.
Do not append a special case for each mistake.
Apply each principle now, unasked, to open work, memory, and procedures.
State it in `core.md` so that later processes apply it to whatever they touch next (the boy scout rule).

## Give each item one home

Take each short-term entry:

- Keep open work in short-term memory.
- Put a general rule in long-term memory, with principles needed every turn in `core.md`.
- Put a specific fact or procedure in external memory under its theme, including facts the owner taught or corrected.
- Delete finished work, logs of past reports, and superseded facts.

Read the destination file or page in full first.
Add the item, update the entry that covers it, delete entries it makes obsolete, or change nothing.

## Restructure

- Organize external memory by theme so that anyone can find a fact by its topic.
- Give a theme with several pages a hub page that links them. Merge duplicates.
- Merge another person's page with yours only when they obviously duplicate each other. Propose other changes to their pages.
- Merge rules that say the same thing. Move a rule that only one procedure needs into that procedure. Delete obsolete rules.
- Keep in procedures/rest.md only the steps that this procedure lacks. Delete any step there that repeats one here.
- Measure files with `wc -c`. Split a long-term or external file over 8 KB by theme.
- Shorten `core.md` or `short-term.md` over 8 KB by moving detail to where it belongs.
- Commit each change separately. Keep edits small because whole-document rewrites lose detail.

## Check and report

Read the diff of your commits from this run and the external pages you changed.
Check that every deleted open item is done or moved, and every deleted fact is obsolete or kept elsewhere.
Restore anything else.

Leave `owner/`, tools, and the harness unchanged, and propose changes to the owner.

If you merged other people's pages or have new proposals, send the owner a brief report.
Use the messaging adapter's send tool and the owner DM channel in its notes.
Record each sent proposal in short-term memory until the owner decides or it no longer applies.
With nothing new, post nothing.

## Rest

Call the core `rest` tool, then end the turn.
The process closes once no turn, waiting input, or native background work remains.
Inputs that arrive before it closes still run in the current `main` process.
