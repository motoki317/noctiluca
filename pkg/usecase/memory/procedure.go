package memory

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

// Original text: MIT (LICENSE). Research ideas: ACE, Reflexion, Letta, and Mem0.
// Sources retain their own licenses. See THIRD_PARTY_NOTICES.md.
//
//go:embed rest.md
var restFrame string

func Procedure(store domain.MemoryStore) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		if name == "rest" {
			return restFrame, nil
		}
		text, err := store.Read(ctx, "procedures/"+name+".md")
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(text), err
	}
}
