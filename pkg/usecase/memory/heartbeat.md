# Check open work

The heartbeat makes sure that no open work is forgotten.

If procedures/heartbeat.md exists in memory, read it first. Each of its steps says where it fits in this procedure, or quotes the sentence here that it replaces. Write a step that replaces a sentence here only when the owner asks for it.

## Check each item

Read `short-term.md`. If it is missing or lists no open work, end the turn without checking any source.

Skip an item whose `status:` line says `Next check: <time or event>` until then. The wait is not a lack of progress.
Check every item that you do not skip at the source it links, such as a conversation, a pull request, or a ticket.
Judge whether the item is done by the done condition written in it. Delete the item when it is done or cancelled.

## Report

Report an item that is blocked, waits on someone, or has made no progress for a day.
Do not report an item again on the same day unless its state changed.
Post each report in the item's conversation.
For an item without a conversation, use the tool and channel that an "Owner DM" line in the messaging notes names.

## Record

Keep one `status:` line per item, with its latest state, when that state last changed, and when you last reported the item.
Compaction and rest drop what earlier runs saw, so put in that line anything else that a later run needs.
When nothing can change before a known time or event, end the line with `Next check: <time or event>`.
Delete any step in procedures/heartbeat.md that only repeats a step here.
Commit your edits in one commit.

If nothing needs a report, post nothing.
