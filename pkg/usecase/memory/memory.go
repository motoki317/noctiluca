package memory

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func Prompt(memory domain.MemoryStore, notes string, next domain.PromptAssembler) domain.PromptAssembler {
	return func(ctx context.Context, session domain.ToolSession, input domain.UserInput) (domain.UserInput, error) {
		if next != nil {
			var err error
			input, err = next(ctx, session, input)
			if err != nil {
				return input, err
			}
		}
		var context strings.Builder
		if memory != nil {
			names, err := memory.List(ctx, "owner")
			if err != nil {
				return input, err
			}
			for _, name := range append(names, "core.md") {
				text, err := memory.Read(ctx, name)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return input, err
				}
				if text != "" {
					context.WriteString("Memory: " + name + "\n" + text + "\n\n")
				}
			}
		}
		if notes != "" {
			context.WriteString("Messaging tool notes:\n" + notes + "\n\n")
		}
		if context.Len() > 0 {
			input.Text = context.String() + "Input:\n" + input.Text
		}
		return input, nil
	}
}
