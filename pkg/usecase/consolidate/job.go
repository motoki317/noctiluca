package consolidate

import "embed"

//go:embed procedures/*.md
var Procedures embed.FS

type Config struct {
	Enabled bool   `mapstructure:"enabled"`
	Cron    string `mapstructure:"cron"`
	TZ      string `mapstructure:"tz"`
}
