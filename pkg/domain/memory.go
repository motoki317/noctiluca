package domain

import "context"

type MemoryMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type MemoryStore interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Read(ctx context.Context, path string) (string, error)
	Write(ctx context.Context, path, content, message string) error
	Search(ctx context.Context, query string) ([]MemoryMatch, error)
}

type MemoryLimits struct {
	CoreBytes  int64 `json:"core_bytes" mapstructure:"core_bytes"`
	FileBytes  int64 `json:"file_bytes" mapstructure:"file_bytes"`
	TotalBytes int64 `json:"total_bytes" mapstructure:"total_bytes"`
	FileCount  int   `json:"file_count" mapstructure:"file_count"`
}

type MemoryFileSize struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type MemorySizes struct {
	Revision   string           `json:"revision"`
	CoreBytes  int64            `json:"core_bytes"`
	Files      []MemoryFileSize `json:"files"`
	TotalBytes int64            `json:"total_bytes"`
	FileCount  int              `json:"file_count"`
	Limits     MemoryLimits     `json:"limits"`
}

type MemorySnapshot struct {
	Store MemoryStore
	Dir   string
	Close func() error
}

type MemoryHistory interface {
	MemoryStore
	Revision(context.Context) (string, error)
	Sizes(context.Context) (MemorySizes, error)
	Snapshot(context.Context, string) (MemorySnapshot, error)
	Revert(context.Context, string) (string, error)
	WriteRevision(ctx context.Context, path, content, message string) (string, error)
	Delete(ctx context.Context, path, message string) (string, error)
}
