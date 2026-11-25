package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/usecase/reconcile"
	"github.com/spf13/viper"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/infrastructure/traq"
)

type secretRef struct {
	Env  string `mapstructure:"env"`
	File string `mapstructure:"file"`
	// Unknown collects unexpected keys before the decoder names them in its error.
	// A token pasted over `env: NAME` becomes a key and would leak there.
	Unknown map[string]any `mapstructure:",remain"`
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

type trustConfig struct {
	Users  []string `mapstructure:"users"`
	Groups []string `mapstructure:"groups"`
	Bots   []string `mapstructure:"bots"`
}

func (t *trustConfig) rule() domain.AcceptRule {
	if t == nil {
		return domain.AcceptRule{AnySender: true}
	}
	return domain.AcceptRule{Users: t.Users, Groups: t.Groups, Bots: t.Bots}
}

type conversationConfig struct {
	ID             string       `mapstructure:"id"`
	Ignore         bool         `mapstructure:"ignore"`
	RequireMention bool         `mapstructure:"require_mention"`
	Trust          *trustConfig `mapstructure:"trust"`
}

func (c conversationConfig) rule(entry *trustConfig) domain.AcceptRule {
	if c.Trust != nil {
		entry = c.Trust
	}
	r := entry.rule()
	r.Ignore, r.RequireMention = c.Ignore, c.RequireMention
	return r
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
	TraQ struct {
		Origin      string    `mapstructure:"origin"`
		AccessToken secretRef `mapstructure:"access_token"`
	} `mapstructure:"traq"`
	Trust         *trustConfig         `mapstructure:"trust"`
	Conversations []conversationConfig `mapstructure:"conversations"`
}

const defaultGroupRefreshInterval = time.Hour

func (c messagingConfig) groupRefreshInterval() time.Duration {
	if c.Slack.GroupRefreshInterval == nil {
		return defaultGroupRefreshInterval
	}
	return *c.Slack.GroupRefreshInterval
}

func (c *messagingConfig) tokenRefs() []*secretRef {
	return []*secretRef{&c.Slack.AppToken, &c.Slack.BotToken, &c.Slack.UserToken, &c.Discord.Token, &c.TraQ.AccessToken}
}

type messagingTokens struct {
	Slack           slack.Config
	DiscordToken    string
	TraQAccessToken string
}

type cronConfig struct {
	Name    string `mapstructure:"name"`
	Cron    string `mapstructure:"cron"`
	TZ      string `mapstructure:"tz"`
	Prompt  string `mapstructure:"prompt"`
	Workdir string `mapstructure:"workdir"`
}

type mcpServerConfig struct {
	mcpclient.ServerConfig `mapstructure:",squash"`
	ClientSecretRef        secretRef `mapstructure:"client_secret"`
}

type Config struct {
	Rest struct {
		Enabled bool   `mapstructure:"enabled"`
		Cron    string `mapstructure:"cron"`
		TZ      string `mapstructure:"tz"`
	} `mapstructure:"rest"`
	Messaging []messagingConfig `mapstructure:"messaging"`
	Model     struct {
		Type            string `mapstructure:"type"`
		Workdir         string `mapstructure:"workdir"`
		Executable      string `mapstructure:"executable"`
		Name            string `mapstructure:"name"`
		ReasoningEffort string `mapstructure:"reasoning_effort"`
	} `mapstructure:"model"`
	Memory struct {
		Dir string `mapstructure:"dir"`
	} `mapstructure:"memory"`
	Database repository.Config `mapstructure:"database"`
	ToolHub  struct {
		Listen string `mapstructure:"listen"`
	} `mapstructure:"toolhub"`
	Events struct {
		Listen string    `mapstructure:"listen"`
		Token  secretRef `mapstructure:"token"`
	} `mapstructure:"events"`
	Heartbeat struct {
		Interval time.Duration `mapstructure:"interval"`
		Enabled  bool          `mapstructure:"enabled"`
	} `mapstructure:"heartbeat"`
	Cron       []cronConfig      `mapstructure:"cron"`
	MCPServers []mcpServerConfig `mapstructure:"mcp_servers"`
	Log        struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
	path     string
	logLevel slog.Level
}

type labeledTokenRef struct {
	ref         *secretRef
	formatError string
}

func (c *Config) tokenRefs() []labeledTokenRef {
	var refs []labeledTokenRef
	for i := range c.Messaging {
		formatError := fmt.Sprintf("invalid config: messaging entry %d: write each token as {env: NAME} or {file: path}", i+1)
		for _, ref := range c.Messaging[i].tokenRefs() {
			refs = append(refs, labeledTokenRef{ref: ref, formatError: formatError})
		}
	}
	for i := range c.MCPServers {
		refs = append(refs, labeledTokenRef{
			ref:         &c.MCPServers[i].ClientSecretRef,
			formatError: fmt.Sprintf("invalid config: MCP server entry %d: write client_secret as {env: NAME} or {file: path}", i+1),
		})
	}
	return append(refs, labeledTokenRef{
		ref:         &c.Events.Token,
		formatError: "invalid config: write events.token as {env: NAME} or {file: path}",
	})
}

func (c Config) mcpServerConfigs() []mcpclient.ServerConfig {
	var servers []mcpclient.ServerConfig
	for _, config := range c.MCPServers {
		servers = append(servers, config.ServerConfig)
	}
	return servers
}

func loadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	for key, value := range map[string]any{
		"log.level": "INFO", "database.path": "noctiluca.db", "memory.dir": "memory",
		"model.type": "claude", "model.workdir": "", "model.name": "",
		"toolhub.listen": "127.0.0.1:0", "heartbeat.enabled": true, "heartbeat.interval": "1h",
		"rest.enabled": false, "rest.cron": "0 4 * * *", "rest.tz": "UTC",
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
	entries, err := validateConfigShape(v)
	if err != nil {
		return Config{}, err
	}
	v.SetDefault("model.reasoning_effort", "")
	var config Config
	// The decoder error names fields and unknown keys and omits values, so it is safe to return.
	// An invalid duration is the exception: its error keeps the value up to its last space. Durations hold no secrets.
	if err := v.UnmarshalExact(&config); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	validateTrust := func(fields map[string]any, location string) error {
		if trust, present := fields["trust"]; present && trust == nil {
			return fmt.Errorf("invalid config: %s.trust cannot be null: omit trust (entries accept any sender, overrides inherit the entry rule) or use trust: {} for the owner only", location)
		}
		return nil
	}
	// The decoded struct cannot tell an absent key from an explicit false or null, so check the raw keys.
	for i, instance := range config.Messaging {
		entry, _ := entries[i].(map[string]any)
		if err := validateTrust(entry, fmt.Sprintf("messaging entry %d", i+1)); err != nil {
			return Config{}, err
		}
		overrides, _ := entry["conversations"].([]any)
		for j, override := range instance.Conversations {
			fields, _ := overrides[j].(map[string]any)
			if override.Ignore && len(fields) > 2 {
				return Config{}, fmt.Errorf("invalid config: messaging entry %d conversations[%d] ignore: true permits only id and ignore", i+1, j)
			}
			if err := validateTrust(fields, fmt.Sprintf("messaging entry %d conversations[%d]", i+1, j)); err != nil {
				return Config{}, err
			}
		}
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
	for _, token := range config.tokenRefs() {
		if len(token.ref.Unknown) != 0 {
			return Config{}, errors.New(token.formatError)
		}
		token.ref.File = absolute(token.ref.File)
	}
	for i, server := range config.MCPServers {
		ref := server.ClientSecretRef
		if server.ClientID == "" && (ref.Env != "" || ref.File != "") {
			return Config{}, fmt.Errorf("invalid config: MCP server entry %d: client_secret requires client_id", i+1)
		}
	}
	if strings.ContainsRune(config.Model.Executable, filepath.Separator) {
		config.Model.Executable = absolute(config.Model.Executable)
	}
	for i := range config.Cron {
		config.Cron[i].Workdir = absolute(config.Cron[i].Workdir)
	}
	return config, mcpclient.Validate(config.mcpServerConfigs())
}

func validateConfigShape(v *viper.Viper) ([]any, error) {
	// UnmarshalExact does not reject a key whose value is null or an empty map, so check these keys by presence.
	keys := v.AllKeys()
	for _, key := range []string{"owner", "slack", "conversations"} {
		if v.InConfig(key) || slices.Contains(keys, key) {
			return nil, errors.New("invalid config: move owner, slack, and conversations into messaging entries")
		}
	}
	_, effortEnv := os.LookupEnv("NOCTILUCA_MODEL_REASONING_EFFORT")
	if v.GetString("model.type") != "codex" && (v.InConfig("model.reasoning_effort") || slices.Contains(keys, "model.reasoning_effort") || effortEnv) {
		return nil, errors.New("model.reasoning_effort requires model.type codex")
	}
	rawMessaging := v.Get("messaging")
	entries, list := rawMessaging.([]any)
	if rawMessaging != nil && !list {
		return nil, errors.New("invalid config: messaging must be a list of entries")
	}
	for i, raw := range entries {
		entry, _ := raw.(map[string]any)
		if _, mapping := entry["conversations"].(map[string]any); mapping {
			return nil, errors.New("invalid config: replace conversations.direct_messages and conversations.rules with entry trust.users, trust.groups, trust.bots, and a conversations list of overrides")
		}
		trust, _ := entry["trust"].(map[string]any)
		fields := []map[string]any{entry, trust}
		overrides, _ := entry["conversations"].([]any)
		for _, raw := range overrides {
			override, _ := raw.(map[string]any)
			trust, _ := override["trust"].(map[string]any)
			fields = append(fields, override, trust)
		}
		for _, field := range fields {
			if _, present := field["allow_bots"]; present {
				return nil, errors.New("invalid config: replace allow_bots with trust.bots listing bot sender IDs")
			}
		}
		for _, platform := range []string{"slack", "discord", "traq"} {
			if _, present := entry[platform]; present && entry["type"] != platform {
				return nil, fmt.Errorf("invalid config: messaging entry %d has %s settings but a different type", i+1, platform)
			}
		}
	}
	return entries, nil
}

func loopbackPort(address string, minPort int) (int, bool) {
	host, port, err := net.SplitHostPort(address)
	number, numberErr := strconv.Atoi(port)
	return number, err == nil && host == "127.0.0.1" && numberErr == nil && number >= minPort && number <= 65535
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
	if c.Heartbeat.Interval <= 0 {
		return nil, 0, errors.New("heartbeat.interval must be positive")
	}
	port, ok := loopbackPort(c.ToolHub.Listen, 0)
	if !ok {
		return nil, 0, errors.New("toolhub.listen must be 127.0.0.1:<port>, with port 0 through 65535")
	}
	if c.Events.Listen != "" {
		if _, ok := loopbackPort(c.Events.Listen, 1); !ok {
			return nil, 0, errors.New("events.listen must be 127.0.0.1:<port>, with port 1 through 65535")
		}
	}
	if err := c.validateWorkdir(); err != nil {
		return nil, 0, err
	}
	if c.Rest.Enabled {
		if err := reconcile.ValidateCron(c.Rest.Cron); err != nil {
			return nil, 0, err
		}
	}
	for _, job := range c.Cron {
		if err := reconcile.ValidateCron(job.Cron); err != nil {
			return nil, 0, err
		}
		if reconcile.IsBuiltInProcedure(job.Name) {
			return nil, 0, fmt.Errorf("%s and %s are reserved cron names", domain.HeartbeatSchedule, domain.RestSchedule)
		}
	}
	tokens := make([]messagingTokens, len(c.Messaging))
	for i, instance := range c.Messaging {
		seen := map[string]bool{}
		for _, rule := range instance.Conversations {
			if rule.ID == "" || strings.ContainsAny(rule.ID, ": \t\r\n") || seen[rule.ID] {
				return nil, 0, fmt.Errorf("messaging.%s conversation overrides require unique channel IDs", instance.Name)
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
			if err := instance.validateSlackIDs(); err != nil {
				return nil, 0, err
			}
			if instance.groupRefreshInterval() <= 0 {
				return nil, 0, fmt.Errorf("messaging.%s.slack.group_refresh_interval must be positive", instance.Name)
			}
			fields = []tokenField{
				{"app_token", instance.Slack.AppToken, &tokens[i].Slack.AppToken, true},
				{"bot_token", instance.Slack.BotToken, &tokens[i].Slack.BotToken, true},
				{"user_token", instance.Slack.UserToken, &tokens[i].Slack.UserToken, false},
			}
		case "discord":
			if err := instance.validateDiscordIDs(); err != nil {
				return nil, 0, err
			}
			fields = []tokenField{{"token", instance.Discord.Token, &tokens[i].DiscordToken, true}}
		case "traq":
			if err := traq.ValidateOrigin(instance.TraQ.Origin); err != nil {
				return nil, 0, fmt.Errorf("messaging.%s.traq.origin %w", instance.Name, err)
			}
			if err := instance.validateTraQIDs(); err != nil {
				return nil, 0, err
			}
			fields = []tokenField{{"access_token", instance.TraQ.AccessToken, &tokens[i].TraQAccessToken, true}}
		default:
			return nil, 0, fmt.Errorf("messaging.%s.type must be slack, discord, or traq", instance.Name)
		}
		for _, field := range fields {
			value, err := field.source.read(field.required)
			if err != nil {
				return nil, 0, fmt.Errorf("messaging.%s.%s.%s: %w", instance.Name, instance.Type, field.name, err)
			}
			*field.target = value
		}
	}
	return tokens, port, nil
}

var slackConversationIDPattern = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)

func (c messagingConfig) validateSlackIDs() error {
	for i, rule := range c.Conversations {
		if !slackConversationIDPattern.MatchString(rule.ID) {
			return fmt.Errorf("messaging.%s.conversations[%d].id must be a Slack channel ID matching %s", c.Name, i, slackConversationIDPattern)
		}
	}
	return nil
}

func (c messagingConfig) validateDiscordIDs() error {
	for i, rule := range c.Conversations {
		id, err := strconv.ParseUint(rule.ID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != rule.ID {
			return fmt.Errorf("messaging.%s.conversations[%d].id must be a Discord numeric ID", c.Name, i)
		}
	}
	return nil
}

func (c messagingConfig) validateTraQIDs() error {
	fields := map[string][]string{"owner": {c.Owner}}
	addTrust := func(prefix string, trust *trustConfig) {
		if trust == nil {
			return
		}
		fields[prefix+"users"] = trust.Users
		fields[prefix+"groups"] = trust.Groups
		fields[prefix+"bots"] = trust.Bots
	}
	addTrust("trust.", c.Trust)
	for i, rule := range c.Conversations {
		prefix := fmt.Sprintf("conversations[%d].", i)
		fields[prefix+"id"] = []string{rule.ID}
		addTrust(prefix+"trust.", rule.Trust)
	}
	for field, ids := range fields {
		for _, id := range ids {
			parsed, err := uuid.Parse(id)
			if err != nil || parsed.String() != id {
				return fmt.Errorf("messaging.%s.%s must contain UUIDs in lowercase canonical form", c.Name, field)
			}
		}
	}
	return nil
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
	for _, token := range c.tokenRefs() {
		paths = append(paths, token.ref.File)
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
