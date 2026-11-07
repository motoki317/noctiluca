package consolidate

import (
	"context"
	"embed"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

//go:embed procedures/*.md
var Procedures embed.FS

type Config struct {
	Enabled           bool     `mapstructure:"enabled"`
	Cron              string   `mapstructure:"cron"`
	TZ                string   `mapstructure:"tz"`
	NegativeReactions []string `mapstructure:"negative_reactions"`
}

func Checkpoint(repo domain.EpisodeRepository) func(context.Context, string, time.Time) error {
	return func(ctx context.Context, procedure string, at time.Time) error {
		if procedure == "consolidate" {
			return repo.SaveLastRun(ctx, at)
		}
		return nil
	}
}
