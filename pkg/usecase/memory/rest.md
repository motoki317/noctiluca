# Save and restructure memory

The `main` session holds the context of every conversation, scheduled task, and background result.
After `rest`, a fresh `main` process knows only its system prompt and what it reads from memory.

If procedures/rest.md exists in memory, read it first. Each of its steps says where it fits in this procedure, or quotes the sentence here that it replaces. Write a step that replaces a sentence here only when the owner asks for it.

## Save what the next process needs

Read `short-term.md` in full. Add what it lacks:

- Open work, each in an entry as the memory notes describe.
- Pending `watch` commands and `wake_at` notes, and what each is for.

Save the facts learned across conversations, tasks, and background results under their theme in external memory.
Skip what memory already holds.

## Learn general principles

Read `core.md` and the relevant long-term files before you edit them.
Compress the lessons in what the owner taught or corrected into general principles in `core.md`.
Keep supporting detail in `long-term/`. Link to it from `core.md`.
Infer what the owner really wanted, beyond the literal correction.
If a correction shows that a principle is worded too narrowly, broaden that principle to cover the intent.
Do not append a special case for each mistake.
Apply each new or broadened principle now, unasked, to open work, memory, and procedures.
State it in `core.md` so that later processes apply it to whatever they touch next (the boy scout rule).

## Give each item one home

Take each short-term entry:

- Keep open work in short-term memory.
- Put a general rule in long-term memory. Put a principle that every turn needs in `core.md`.
- Put a specific fact or procedure in external memory under its theme. This includes specific facts that the owner taught or corrected.
- Delete finished work, logs of past reports, and superseded facts.

Read the destination file or page in full first.
Then add the item, update the entry that covers it, delete entries that it makes obsolete, or change nothing.

## Restructure

- Organize external memory by theme, so that anyone can find a fact by its topic.
- Give a theme with several pages a hub page that links them. Merge duplicate pages.
- Merge another person's page with yours only when the two obviously duplicate each other. Propose other changes to their pages.
- Merge rules that say the same thing. Move a rule that only one procedure needs into that procedure. Delete obsolete rules.
- Delete any step in procedures/rest.md that only repeats a step here.
- Measure files with `wc -c`. Split a long-term or external file over 8 KB by theme.
- Move detail out of `core.md` and `short-term.md` to where it belongs.
- Commit each change separately. Keep edits small, because whole-document rewrites lose detail.

## Check and report

Read the diff of your commits from this run and the external pages you changed.
Check that every deleted open item is done or moved.
Check that every deleted fact is obsolete or kept elsewhere.
Restore anything else.

Do not change `owner/`, tools, or the harness yourself. Propose such changes to the owner.

If you merged other people's pages or have new proposals, send the owner a brief report.
Send it with the tool and channel that an "Owner DM" line in the messaging notes names.
With nothing new, post nothing.

## Rest

Call the `rest` tool, then end the turn.
The process closes once no turn, waiting input, or native background work remains.
Inputs that arrive before it closes still run in the current `main` process.
