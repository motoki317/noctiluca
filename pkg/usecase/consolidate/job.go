package consolidate

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

//go:embed procedures/*.md
var procedures embed.FS

type Config struct {
	Enabled           bool     `mapstructure:"enabled"`
	Cron              string   `mapstructure:"cron"`
	TZ                string   `mapstructure:"tz"`
	NegativeReactions []string `mapstructure:"negative_reactions"`
}

type procedureMemory struct{ domain.MemoryHistory }

func WithProcedures(memory domain.MemoryHistory) domain.MemoryHistory {
	return &procedureMemory{memory}
}

func (m *procedureMemory) Read(ctx context.Context, path string) (string, error) {
	text, err := m.MemoryHistory.Read(ctx, path)
	if !errors.Is(err, fs.ErrNotExist) {
		return text, err
	}
	data, defaultErr := procedures.ReadFile(path)
	if defaultErr != nil {
		return text, err
	}
	return string(data), nil
}

func (m *procedureMemory) missing(ctx context.Context) ([]string, error) {
	var names []string
	for _, name := range []string{"procedures/consolidate.md", "procedures/hillclimb.md"} {
		if _, err := m.MemoryHistory.Read(ctx, name); errors.Is(err, fs.ErrNotExist) {
			names = append(names, name)
		} else if err != nil {
			return nil, err
		}
	}
	return names, nil
}

func (m *procedureMemory) List(ctx context.Context, prefix string) ([]string, error) {
	names, err := m.MemoryHistory.List(ctx, prefix)
	if err != nil || (prefix != "" && prefix != "procedures") {
		return names, err
	}
	defaults, err := m.missing(ctx)
	names = append(names, defaults...)
	slices.Sort(names)
	return names, err
}

func (m *procedureMemory) Search(ctx context.Context, query string) ([]domain.MemoryMatch, error) {
	matches, err := m.MemoryHistory.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	names, err := m.missing(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		text, err := m.Read(ctx, name)
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, query) {
				matches = append(matches, domain.MemoryMatch{Path: name, Line: i + 1, Text: line})
			}
		}
	}
	return matches[:min(len(matches), 100)], nil
}

func Checkpoint(repo domain.EpisodeRepository) func(context.Context, string, time.Time) error {
	return func(ctx context.Context, procedure string, at time.Time) error {
		if procedure == "consolidate" {
			return repo.SaveLastRun(ctx, at)
		}
		return nil
	}
}
