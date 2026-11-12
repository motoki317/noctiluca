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
				"- owner/ files and core.md are in this system prompt. Edits take effect in the next model process.\n"+
				"- Everything outside owner/ is your own memory, in three tiers. Keep each file under 8 KB.\n"+
				"  - Short-term, short-term.md: the exact state of open work, such as requests and promised follow-ups. Delete an entry when its work ends.\n"+
				"  - Long-term, core.md and long-term/: rules generalized from repeated experience, with few specific facts. core.md holds what every turn needs, an index of long-term/, and pointers to at most ten theme hubs: pages that link the pages of one external theme.\n"+
				"  - External: specific facts and procedures, organized by theme. Put what may be shared in the knowledge system the owner connected, if any, and private ones under external/. procedures/ holds the external procedures that noctiluca's scheduled jobs read.\n"+
				"- Commit each change with a message that says why.\n"+
				"- Commit only paths you changed because other sessions share this repository.\n"+
				"- Retry if git reports a lock.\n", memory.Dir())
			if hasRemote {
				context.WriteString("- The memory repository pushes to a remote backup that nothing else writes. Push after each commit. When a push is rejected, run `git push --force-with-lease`; never pull. Tell the owner when a push fails for another reason.\n")
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
			context.WriteString("Messaging tool notes:\n" + notes + "\n\n")
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
