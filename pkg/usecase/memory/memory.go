package memory

import (
	"context"
	"encoding/json"
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
			fmt.Fprintf(&context, "Memory notes:\n"+
				"- Read and edit memory in %q with your own file tools.\n"+
				"- owner/ holds the owner's handwritten files. Never edit, move, or delete them. Propose fixes to the owner.\n"+
				"- owner/ files and core.md are in this system prompt. Edits take effect in the next model process.\n"+
				"- Keep core.md under 8 KB.\n"+
				"- Everything outside owner/, including core.md, is your own memory.\n"+
				"- Commit each change with a message that says why.\n"+
				"- Commit only paths you changed because other sessions share this repository.\n"+
				"- Retry if git reports a lock.\n\n", memory.Dir())
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

func Tools(memory domain.MemoryStore) []domain.Tool {
	tools := []domain.Tool{
		{Name: "memory_read", ReadOnly: true, Description: "Read a memory file by relative path.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1}},"required":["path"],"additionalProperties":false}`), Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, err
			}
			text, err := memory.Read(ctx, input.Path)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]string{"path": input.Path, "text": text})
		}},
		{Name: "memory_write", Description: "Write a memory file, or delete it with delete=true instead of content. Commit with your message. owner/ is read-only and edited by hand. Write any other relative path. Size caps reject growth; merge or delete to fit.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"delete":{"type":"boolean"},"message":{"type":"string","minLength":1}},"required":["path","message"],"additionalProperties":false}`), Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
				Delete  bool    `json:"delete"`
				Message string  `json:"message"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, err
			}
			if input.Delete == (input.Content != nil) {
				return nil, errors.New("provide content or delete=true, exclusively")
			}
			var revision string
			var err error
			if history, ok := memory.(domain.MemoryHistory); ok {
				if input.Delete {
					revision, err = history.Delete(ctx, input.Path, input.Message)
				} else {
					revision, err = history.WriteRevision(ctx, input.Path, *input.Content, input.Message)
				}
			} else if input.Delete {
				return nil, errors.New("memory deletion is unavailable")
			} else {
				err = memory.Write(ctx, input.Path, *input.Content, input.Message)
			}
			if err != nil {
				return nil, err
			}
			result := map[string]string{"path": input.Path}
			if revision != "" {
				result["revision"] = revision
			}
			return json.Marshal(result)
		}},
		{Name: "memory_search", ReadOnly: true, Description: "Find literal text in memory files. Returns at most 100 matching lines.", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1}},"required":["query"],"additionalProperties":false}`), Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, err
			}
			matches, err := memory.Search(ctx, input.Query)
			if err != nil {
				return nil, err
			}
			if matches == nil {
				matches = []domain.MemoryMatch{}
			}
			return json.Marshal(matches)
		}},
	}
	if history, ok := memory.(domain.MemoryHistory); ok {
		tools = append(tools, domain.Tool{
			Name: "memory_sizes", ReadOnly: true, Description: "Report the memory revision, core bytes, per-file bytes, total bytes, file count, and configured caps.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Handler: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				sizes, err := history.Sizes(ctx)
				if err != nil {
					return nil, err
				}
				return json.Marshal(sizes)
			},
		}, domain.Tool{
			Name: "memory_revert", Description: "Revert one memory commit while preserving later unrelated edits. Commits that touch owner/ cannot be reverted. Conflicts leave memory unchanged.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"commit":{"type":"string","minLength":1}},"required":["commit"],"additionalProperties":false}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var input struct {
					Commit string `json:"commit"`
				}
				if err := json.Unmarshal(raw, &input); err != nil {
					return nil, err
				}
				revision, err := history.Revert(ctx, input.Commit)
				if err != nil {
					return nil, err
				}
				return json.Marshal(map[string]string{"revision": revision})
			},
		})
	}
	return tools
}
