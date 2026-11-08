package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/spf13/viper"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
)

type secretRef struct {
	Env  string `mapstructure:"env"`
	File string `mapstructure:"file"`
}

func (s secretRef) read(required bool) (string, error) {
	if s.Env != "" && s.File != "" {
		return "", errors.New("choose env or file, not both")
	}
	var value string
	if s.Env != "" {
		value = os.Getenv(s.Env)
	}
	if s.File != "" {
		data, err := os.ReadFile(s.File)
		if err != nil {
			return "", errors.New("cannot read token file")
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" && (required || s.Env != "" || s.File != "") {
		return "", errors.New("token source is missing or empty")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("token contains whitespace")
	}
	return value, nil
}

type acceptConfig struct {
	Users          []string `mapstructure:"users"`
	Groups         []string `mapstructure:"groups"`
	AllowBots      bool     `mapstructure:"allow_bots"`
	RequireMention bool     `mapstructure:"require_mention"`
}

func (r acceptConfig) rule() domain.AcceptRule {
	return domain.AcceptRule{Users: r.Users, Groups: r.Groups, AllowBots: r.AllowBots, RequireMention: r.RequireMention}
}

type messagingConfig struct {
	Name  string `mapstructure:"name"`
	Type  string `mapstructure:"type"`
	Owner string `mapstructure:"owner"`
	Slack struct {
		AppToken             secretRef      `mapstructure:"app_token"`
		BotToken             secretRef      `mapstructure:"bot_token"`
		UserToken            secretRef      `mapstructure:"user_token"`
		GroupRefreshInterval *time.Duration `mapstructure:"group_refresh_interval"`
	} `mapstructure:"slack"`
	Discord struct {
		Token secretRef `mapstructure:"token"`
	} `mapstructure:"discord"`
	Conversations struct {
		DirectMessages *acceptConfig `mapstructure:"direct_messages"`
		Rules          []struct {
			ID           string `mapstructure:"id"`
			acceptConfig `mapstructure:",squash"`
		} `mapstructure:"rules"`
	} `mapstructure:"conversations"`
}

func (c messagingConfig) groupRefreshInterval() time.Duration {
	if c.Type != "slack" || c.Slack.GroupRefreshInterval == nil {
		return time.Hour
	}
	return *c.Slack.GroupRefreshInterval
}

func (c *messagingConfig) tokenRefs() []*secretRef {
	return []*secretRef{&c.Slack.AppToken, &c.Slack.BotToken, &c.Slack.UserToken, &c.Discord.Token}
}

type messagingTokens struct {
	Slack        slack.Config
	DiscordToken string
}

type Config struct {
	Consolidate consolidate.Config `mapstructure:"consolidate"`
	Messaging   []messagingConfig  `mapstructure:"messaging"`
	Model       struct {
		Type            string        `mapstructure:"type"`
		Workdir         string        `mapstructure:"workdir"`
		TurnTimeout     time.Duration `mapstructure:"turn_timeout"`
		Executable      string        `mapstructure:"executable"`
		Name            string        `mapstructure:"name"`
		ReasoningEffort string        `mapstructure:"reasoning_effort"`
		IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	} `mapstructure:"model"`
	Memory struct {
		Dir string `mapstructure:"dir"`
	} `mapstructure:"memory"`
	Database repository.Config `mapstructure:"database"`
	ToolHub  struct {
		Listen string `mapstructure:"listen"`
	} `mapstructure:"toolhub"`
	Heartbeat struct {
		Interval time.Duration `mapstructure:"interval"`
		Enabled  bool          `mapstructure:"enabled"`
	} `mapstructure:"heartbeat"`
	Tasks struct {
		Timeout time.Duration `mapstructure:"timeout"`
	} `mapstructure:"tasks"`
	Cron       []domain.Schedule        `mapstructure:"cron"`
	MCPServers []mcpclient.ServerConfig `mapstructure:"mcp_servers"`
	Log        struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
	path     string
	logLevel slog.Level
}

func loadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	for key, value := range map[string]any{
		"log.level": "INFO", "database.path": "noctiluca.db", "memory.dir": "memory",
		"model.type": "claude", "model.workdir": "", "model.turn_timeout": "2h", "model.name": "", "model.idle_timeout": "1h",
		"toolhub.listen": "127.0.0.1:0", "heartbeat.enabled": true, "heartbeat.interval": "1h",
		"tasks.timeout":       "30m",
		"consolidate.enabled": false, "consolidate.cron": "0 4 * * *", "consolidate.tz": "UTC",
		"consolidate.negative_reactions": []string{"-1", "thumbsdown"},
	} {
		v.SetDefault(key, value)
	}
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
	switch modelType := v.GetString("model.type"); modelType {
	case "claude", "codex":
		v.SetDefault("model.executable", modelType)
	default:
		return Config{}, errors.New("model.type must be claude or codex")
	}
	// Viper omits empty maps and null values from strict decoding.
	keys := v.AllKeys()
	for _, key := range []string{"owner", "slack", "conversations"} {
		if v.InConfig(key) || slices.Contains(keys, key) {
			return Config{}, errors.New("invalid config: move owner, slack, and conversations into messaging entries")
		}
	}
	_, effortEnv := os.LookupEnv("NOCTILUCA_MODEL_REASONING_EFFORT")
	if v.GetString("model.type") != "codex" && (v.InConfig("model.reasoning_effort") || slices.Contains(keys, "model.reasoning_effort") || effortEnv) {
		return Config{}, errors.New("model.reasoning_effort requires model.type codex")
	}
	v.SetDefault("model.reasoning_effort", "")
	entries, _ := v.Get("messaging").([]any)
	for i, raw := range entries {
		entry, _ := raw.(map[string]any)
		for _, platform := range []string{"slack", "discord"} {
			if _, present := entry[platform]; present && entry["type"] != platform {
				return Config{}, fmt.Errorf("invalid config: messaging entry %d has %s settings but a different type", i+1, platform)
			}
		}
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
	config.Memory.Dir = absolute(config.Memory.Dir)
	config.Model.Workdir = absolute(config.Model.Workdir)
	if config.Model.Workdir == "" {
		config.Model.Workdir = filepath.Join(filepath.Dir(config.Database.Path), "work")
	}
	for i := range config.Messaging {
		for _, ref := range config.Messaging[i].tokenRefs() {
			ref.File = absolute(ref.File)
		}
	}
	if strings.ContainsRune(config.Model.Executable, filepath.Separator) {
		config.Model.Executable = absolute(config.Model.Executable)
	}
	for i := range config.Cron {
		config.Cron[i].Workdir = absolute(config.Cron[i].Workdir)
	}
	return config, mcpclient.Validate(config.MCPServers)
}

func (c Config) validateRun() ([]messagingTokens, int, error) {
	if len(c.Messaging) == 0 {
		return nil, 0, errors.New("messaging requires at least one entry")
	}
	names := map[string]bool{}
	for _, instance := range c.Messaging {
		if !domain.ValidMessagingName(instance.Name) || names[instance.Name] {
			return nil, 0, errors.New("messaging names must be unique and match ^[a-z][a-z0-9_]*$")
		}
		names[instance.Name] = true
		if strings.TrimSpace(instance.Owner) == "" {
			return nil, 0, fmt.Errorf("messaging.%s.owner is required", instance.Name)
		}
	}
	if c.Memory.Dir == "" || c.Database.Path == "" || c.Model.Executable == "" {
		return nil, 0, errors.New("memory.dir, database.path, and model.executable are required")
	}
	if c.Model.TurnTimeout <= 0 || c.Tasks.Timeout <= 0 || c.Model.IdleTimeout <= 0 || c.Heartbeat.Interval <= 0 {
		return nil, 0, errors.New("timeouts and intervals must be positive")
	}
	host, port, err := net.SplitHostPort(c.ToolHub.Listen)
	number, numberErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || numberErr != nil || number < 0 || number > 65535 {
		return nil, 0, errors.New("toolhub.listen must be 127.0.0.1:<port>, with port 0 through 65535")
	}
	if err := c.validateWorkdir(); err != nil {
		return nil, 0, err
	}
	if c.Consolidate.Enabled {
		if err := reconcile.ValidateCron(c.Consolidate.Cron); err != nil {
			return nil, 0, err
		}
	}
	for _, job := range c.Cron {
		if err := reconcile.ValidateCron(job.Cron); err != nil {
			return nil, 0, err
		}
		if job.Name == "heartbeat" || job.Name == "consolidate" {
			return nil, 0, errors.New("heartbeat and consolidate are reserved cron names")
		}
	}
	tokens := make([]messagingTokens, len(c.Messaging))
	for i, instance := range c.Messaging {
		seen := map[string]bool{}
		for _, rule := range instance.Conversations.Rules {
			if rule.ID == "" || strings.ContainsAny(rule.ID, ": \t\r\n") || seen[rule.ID] {
				return nil, 0, fmt.Errorf("messaging.%s conversation rules require unique channel IDs", instance.Name)
			}
			seen[rule.ID] = true
		}
		type tokenField struct {
			name     string
			source   secretRef
			target   *string
			required bool
		}
		var fields []tokenField
		switch instance.Type {
		case "slack":
			if instance.groupRefreshInterval() <= 0 {
				return nil, 0, fmt.Errorf("messaging.%s.slack.group_refresh_interval must be positive", instance.Name)
			}
			fields = []tokenField{
				{"app_token", instance.Slack.AppToken, &tokens[i].Slack.AppToken, true},
				{"bot_token", instance.Slack.BotToken, &tokens[i].Slack.BotToken, true},
				{"user_token", instance.Slack.UserToken, &tokens[i].Slack.UserToken, false},
			}
		case "discord":
			fields = []tokenField{{"token", instance.Discord.Token, &tokens[i].DiscordToken, true}}
		default:
			return nil, 0, fmt.Errorf("messaging.%s.type must be slack or discord", instance.Name)
		}
		for _, field := range fields {
			value, err := field.source.read(field.required)
			if err != nil {
				return nil, 0, fmt.Errorf("messaging.%s.%s.%s: %w", instance.Name, instance.Type, field.name, err)
			}
			*field.target = value
		}
	}
	return tokens, number, nil
}

// Resolve existing ancestors so a symlink cannot hide a protected directory.
func canonicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, linkErr := os.Readlink(path); linkErr == nil {
		return "", fmt.Errorf("model.workdir validation cannot resolve dangling symlink %q", path)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	base, err := canonicalPath(parent)
	return filepath.Join(base, filepath.Base(path)), err
}
func (c Config) validateWorkdir() error {
	work, err := canonicalPath(c.Model.Workdir)
	if err != nil {
		return err
	}
	contains := func(base, path string) bool {
		rel, err := filepath.Rel(base, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	paths := []string{c.path, c.Database.Path, c.Memory.Dir}
	for _, instance := range c.Messaging {
		for _, ref := range instance.tokenRefs() {
			paths = append(paths, ref.File)
		}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		protected, err := canonicalPath(path)
		if err != nil {
			return err
		}
		if contains(work, protected) || contains(work, path) || contains(c.Model.Workdir, path) || (path == c.Memory.Dir && (contains(protected, work) || contains(path, work) || contains(path, c.Model.Workdir))) {
			return errors.New("model.workdir must be separate from config, database, token files, and memory")
		}
	}
	return nil
}
