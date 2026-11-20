package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func Prompt(memory domain.MemoryStore, notes string, next domain.PromptAssembler) domain.PromptAssembler {
	return func(ctx context.Context, session domain.ToolSession) (string, error) {
		var context strings.Builder
		if memory != nil {
			hasRemote, err := memory.HasRemote(ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&context, "Memory notes:\n"+
				"- Read and edit memory in %q with your own file tools.\n"+
				"- owner/ holds the owner's handwritten files. Never edit, move, or delete them. Propose fixes to the owner.\n"+
				"- This system prompt includes the owner/ files and core.md. Edits to them take effect in the next model process, after rest or a restart.\n"+
				"- Everything outside owner/ is your own memory, in three tiers. Keep each file under 8 KB.\n"+
				"  - Short-term, short-term.md: the exact state of open work, such as requests, promised follow-ups, questions and offers that await an answer, and things you keep past their task. "+
				"When you take on a task that may outlive this turn, add it at once. Give each entry its date, its requester, the send tool and IDs to answer it with, its done condition, and its latest state. "+
				"Delete an entry when its done condition holds.\n"+
				"  - Long-term, core.md and long-term/: rules generalized from repeated experience, with few specific facts. core.md holds what every turn needs and an index of long-term/. "+
				"It also points to at most ten theme hubs. A theme hub is a page that links the pages of one external theme.\n"+
				"  - External: specific facts and procedures, organized by theme. If the owner connected a knowledge system, put the shareable ones there. Put all others under external/. "+
				"procedures/ holds procedures. The heartbeat and rest schedules send procedures built into noctiluca. procedures/heartbeat.md and procedures/rest.md hold this instance's own steps for them.\n"+
				"- When the owner corrects or extends a rule or procedure in your own memory, or one fails when you follow it, fix it now, in the file where you would look for it.\n"+
				"- A fact in external memory is a snapshot. Before you quote one that may have changed, check its source. If the fact changed, fix it.\n"+
				"- Commit each change with a message that says why.\n"+
				"- Subagents and background commands also commit here. Commit only the paths you changed. "+
				"In a file that they may also be editing, such as short-term.md, stage only your own hunks, and commit only what you staged. Retry if git reports a lock.\n"+
				"- Follow the rest procedure only when the nightly schedule sends it. Call the rest tool only as its last step.\n", memory.Dir())
			if hasRemote {
				context.WriteString("- The memory repository pushes to a remote backup that nothing else writes. Push after each commit. If a push is rejected, run `git push --force-with-lease`. Never pull. If a push fails for another reason, tell the owner.\n")
			}
			context.WriteString("\n")
			names, err := memory.List(ctx, "owner")
			if err != nil {
				return "", err
			}
			for _, name := range append(names, "core.md") {
				text, err := memory.Read(ctx, name)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return "", err
				}
				if text != "" {
					context.WriteString("Memory: " + name + "\n" + text + "\n\n")
				}
			}
		}
		if notes != "" {
			context.WriteString("Session and messaging notes:\n" + notes + "\n\n")
		}
		if next != nil {
			text, err := next(ctx, session)
			if err != nil {
				return "", err
			}
			context.WriteString(text)
		}
		return context.String(), nil
	}
}
