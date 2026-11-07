package domain

import "context"

type MemoryStore interface {
	Dir() string
	List(ctx context.Context, prefix string) ([]string, error)
	Read(ctx context.Context, path string) (string, error)
}
