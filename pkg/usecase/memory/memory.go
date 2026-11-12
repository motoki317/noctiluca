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
				"- Keep core.md under 8 KB.\n"+
				"- Everything outside owner/, including core.md, is your own memory.\n"+
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
