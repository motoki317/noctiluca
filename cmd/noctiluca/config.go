package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/spf13/viper"
)

type Config struct {
	MCPServers []mcpclient.ServerConfig `mapstructure:"mcp_servers"`
	Database   repository.Config        `mapstructure:"database"`
	Log        struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
	path     string
	logLevel slog.Level
}

func loadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetDefault("log.level", "INFO")
	v.SetDefault("database.path", "noctiluca.db")
	v.SetEnvPrefix("NOCTILUCA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.AllowEmptyEnv(true)
	if path == "" {
		v.SetConfigFile("config.yaml")
	} else {
		v.SetConfigFile(path)
	}
	if err := v.ReadInConfig(); err != nil && (path != "" || !errors.Is(err, os.ErrNotExist)) {
		var parseError viper.ConfigParseError
		if errors.As(err, &parseError) {
			return Config{}, errors.New("invalid config: expected a YAML mapping")
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var config Config
	if err := v.UnmarshalExact(&config); err != nil {
		return Config{}, errors.New("invalid config: check field names and value types")
	}
	if err := config.logLevel.UnmarshalText([]byte(config.Log.Level)); err != nil {
		return Config{}, errors.New("invalid log.level: use DEBUG, INFO, WARN, or ERROR, optionally followed by a numeric offset")
	}
	dir, err := filepath.Abs(filepath.Dir(v.ConfigFileUsed()))
	if err != nil {
		return Config{}, err
	}
	config.path = filepath.Join(dir, filepath.Base(v.ConfigFileUsed()))
	absolute := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(dir, path)
	}
	config.Database.Path = absolute(config.Database.Path)
	return config, mcpclient.Validate(config.MCPServers)
}
