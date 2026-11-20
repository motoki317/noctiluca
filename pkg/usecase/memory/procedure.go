package memory

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func Procedure(store domain.MemoryStore) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		text, err := store.Read(ctx, "procedures/"+name+".md")
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(text), err
	}
}
