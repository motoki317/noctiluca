package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
	return path
}

func TestRunConfigFieldsAndPaths(t *testing.T) {
	path := writeConfig(t, `messaging:
  - name: slack
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {file: bot-token}
      group_refresh_interval: 2h
    conversations:
      direct_messages: {users: [U_OWNER, U_MEMBER]}
      rules:
        - id: C_CHANNEL
          users: [U_OWNER]
          groups: [S_GROUP]
          allow_bots: true
          require_mention: true
model: {executable: ./claude, name: haiku, idle_timeout: 2h}
memory: {dir: memories}
database: {path: history.db}
toolhub: {listen: '127.0.0.1:12345'}
heartbeat: {enabled: false, interval: 30m}
cron:
  - {name: daily, cron: '0 9 * * *', tz: UTC, prompt: Check tasks, workdir: work}
`)
	t.Setenv("TEST_APP_TOKEN", "app-secret")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "bot-token"), []byte("bot-secret\n"), 0600))
	c, err := loadConfig(path)
	require.NoError(t, err)
	tokens, port, err := c.validateRun()
	require.NoError(t, err)
	require.Equal(t, "app-secret", tokens[0].Slack.AppToken)
	require.Equal(t, "bot-secret", tokens[0].Slack.BotToken)
	require.Empty(t, tokens[0].Slack.UserToken)
	require.Equal(t, 12345, port)
	require.Equal(t, []string{"U_OWNER", "U_MEMBER"}, c.Messaging[0].Conversations.DirectMessages.Users)
	require.Len(t, c.Messaging[0].Conversations.Rules, 1)
	require.Equal(t, "C_CHANNEL", c.Messaging[0].Conversations.Rules[0].ID)
	require.Equal(t, []string{"S_GROUP"}, c.Messaging[0].Conversations.Rules[0].Groups)
	require.True(t, c.Messaging[0].Conversations.Rules[0].RequireMention)
	require.True(t, c.Messaging[0].Conversations.Rules[0].AllowBots)
	require.False(t, c.Heartbeat.Enabled)
	require.Equal(t, 30*time.Minute, c.Heartbeat.Interval)
	require.Equal(t, 2*time.Hour, c.Model.IdleTimeout)
	require.Equal(t, 2*time.Hour, c.Messaging[0].groupRefreshInterval())
	require.Equal(t, filepath.Join(filepath.Dir(path), "history.db"), c.Database.Path)
	require.Equal(t, filepath.Join(filepath.Dir(path), "memories"), c.Memory.Dir)
	require.Equal(t, filepath.Join(filepath.Dir(path), "claude"), c.Model.Executable)
	require.Equal(t, filepath.Join(filepath.Dir(path), "work"), c.Cron[0].Workdir)
	require.Equal(t, "Check tasks", c.Cron[0].Prompt)
}

func TestSecretReferencesDoNotExposeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("secret-value\n"), 0600))
	t.Setenv("TEST_TOKEN", "secret-value")
	t.Setenv("EMPTY_TOKEN", "")
	for _, tc := range []struct {
		ref      secretRef
		required bool
		want     string
		fails    bool
	}{
		{ref: secretRef{Env: "TEST_TOKEN"}, want: "secret-value"},
		{ref: secretRef{File: path}, want: "secret-value"},
		{}, {required: true, fails: true},
		{ref: secretRef{Env: "EMPTY_TOKEN"}, fails: true},
		{ref: secretRef{File: "missing"}, fails: true},
		{ref: secretRef{Env: "TEST_TOKEN", File: path}, fails: true},
	} {
		value, err := tc.ref.read(tc.required)
		if tc.fails {
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-value")
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		}
	}
	_, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    slack: {bot_token: secret-value}\n"))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-value")
}

func TestRunValidationBeforeResources(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"", "messaging"},
		{"messaging: [{name: slack, type: slack}]\n", "owner"},
		{"messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n", "slack.app_token"},
		{"messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\ntoolhub: {listen: '0.0.0.0:1234'}\n", "toolhub.listen"},
		{"messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\nheartbeat: {interval: -1s}\n", "interval"},
		{"messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    conversations:\n      rules:\n        - {id: C_CHANNEL}\n        - {id: C_CHANNEL}\n", "unique"},
	} {
		c, err := loadConfig(writeConfig(t, tc.yaml))
		require.NoError(t, err)
		_, err = newApplication(t.Context(), c, nil)
		require.ErrorContains(t, err, tc.want)
		require.NoFileExists(t, c.Database.Path)
		require.NoDirExists(t, c.Memory.Dir)
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := loadConfig("../../config.example.yaml")
	require.NoError(t, err)
	t.Setenv("SLACK_APP_TOKEN", "app-example")
	t.Setenv("SLACK_BOT_TOKEN", "bot-example")
	_, _, err = c.validateRun()
	require.NoError(t, err)
	require.Equal(t, []string{c.Messaging[0].Owner}, c.Messaging[0].Conversations.DirectMessages.Users)
}

func TestExplicitEmptyDMRuleDisablesDefault(t *testing.T) {
	c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    conversations:\n      direct_messages: {users: []}\n"))
	require.NoError(t, err)
	require.NotNil(t, c.Messaging[0].Conversations.DirectMessages)
	require.Empty(t, c.Messaging[0].Conversations.DirectMessages.Users)
}

func TestConsolidationConfig(t *testing.T) {
	c, err := loadConfig(writeConfig(t, "model: {name: haiku}\nconsolidate: {enabled: true}\n"))
	require.NoError(t, err)
	require.Equal(t, "0 4 * * *", c.Consolidate.Cron)
	require.Equal(t, "UTC", c.Consolidate.TZ)
	for _, input := range []string{"consolidate: {grader_model: haiku}", "consolidate: {evals: {count: 1}}", "consolidate: {episode_threshold: 100}"} {
		_, err := loadConfig(writeConfig(t, input))
		require.Error(t, err)
	}

}

func TestDisabledReservedSchedulesAreRemoved(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\nheartbeat: {enabled: false}\n"))
	require.NoError(t, err)
	db, err := repository.New(t.Context(), c.Database)
	require.NoError(t, err)
	for _, name := range []string{"heartbeat", "consolidate"} {
		require.NoError(t, repository.NewScheduleRepository(db).Save(t.Context(), domain.Schedule{Name: name, Cron: "0 * * * *", TZ: "UTC"}))
	}
	require.NoError(t, db.Close())
	app, err := newApplication(t.Context(), c, nil)
	require.NoError(t, err)
	defer app.Shutdown(t.Context())
	jobs, err := repository.NewScheduleRepository(app.db).List(t.Context())
	require.NoError(t, err)
	require.Empty(t, jobs)
}

func TestReservedCronNames(t *testing.T) {
	for _, name := range []string{"heartbeat", "consolidate"} {
		c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\ncron:\n  - {name: "+name+", cron: '0 4 * * *', tz: UTC, prompt: run}\n"))
		require.NoError(t, err)
		_, _, err = c.validateRun()
		require.ErrorContains(t, err, "reserved cron names")
	}
}

func TestRemovedMemoryLimitConfigRejected(t *testing.T) {
	_, err := loadConfig(writeConfig(t, "memory: {limits: {core_bytes: 4096}}\n"))
	require.ErrorContains(t, err, "invalid config")
}

func TestModelWorkdirAndTimeoutDefaults(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\nlog: {level: WARN+2}\n"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(filepath.Dir(c.Database.Path), "work"), c.Model.Workdir)
	require.Equal(t, 2*time.Hour, c.Model.TurnTimeout)
	require.Equal(t, 30*time.Minute, c.Tasks.Timeout)
	require.EqualValues(t, 6, c.logLevel)
	app, err := newApplication(t.Context(), c, nil)
	require.NoError(t, err)
	defer app.Shutdown(t.Context())
	stat, err := os.Stat(c.Model.Workdir)
	require.NoError(t, err)
	require.EqualValues(t, 0700, stat.Mode().Perm())
	files, err := os.ReadDir(c.Model.Workdir)
	require.NoError(t, err)
	require.Empty(t, files)
}
func TestModelWorkdirRejectsProtectedPaths(t *testing.T) {
	for _, body := range []string{
		"model: {workdir: .}\n",
		"model: {workdir: private}\ndatabase: {path: private/db}\n",
		"    slack: {bot_token: {file: private/token}}\nmodel: {workdir: private}\n",
		"model: {workdir: private}\nmemory: {dir: private/memory}\n",
		"model: {workdir: memory/work}\n",
		"model: {workdir: alias}\n",
	} {
		t.Run(body, func(t *testing.T) {
			path := writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n"+body)
			require.NoError(t, os.Symlink(filepath.Dir(path), filepath.Join(filepath.Dir(path), "alias")))
			c, err := loadConfig(path)
			require.NoError(t, err)
			_, _, err = c.validateRun()
			require.ErrorContains(t, err, "model.workdir")
			require.NoFileExists(t, c.Database.Path)
		})
	}
}
func TestCronConfigRequiresFiveFields(t *testing.T) {
	for _, expr := range []string{"@daily", "@every 1h", "0 0 4 * * *", "CRON_TZ=UTC 0 4 * * *"} {
		c, err := loadConfig(writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\ncron: [{name: test, cron: '"+expr+"', tz: UTC, prompt: run}]\n"))
		require.NoError(t, err)
		_, _, err = c.validateRun()
		require.ErrorContains(t, err, "five fields")
	}
}

func TestWorkdirRejectsSecretSymlinkInside(t *testing.T) {
	path := writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n    slack: {bot_token: {file: work/token}}\nmodel: {workdir: work}\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "work"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("token"), 0600))
	require.NoError(t, os.Symlink("../secret", filepath.Join(dir, "work", "token")))
	c, err := loadConfig(path)
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "model.workdir")
}

func TestWorkdirRejectsDatabaseSymlinkBeforeCreation(t *testing.T) {
	path := writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\nmodel: {workdir: work}\ndatabase: {path: database-link}\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.Symlink("work/noctiluca.db", filepath.Join(dir, "database-link")))
	c, err := loadConfig(path)
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "model.workdir")
	require.NoFileExists(t, filepath.Join(dir, "work", "noctiluca.db"))
}

func TestWorkdirRejectsDanglingSymlinkWithParentTraversal(t *testing.T) {
	path := writeConfig(t, "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\nmodel: {workdir: other/work}\ndatabase: {path: database-link}\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "other", "child"), 0700))
	require.NoError(t, os.Symlink("other/child", filepath.Join(dir, "alias")))
	require.NoError(t, os.Symlink("alias/../work/db", filepath.Join(dir, "database-link")))
	c, err := loadConfig(path)
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "model.workdir")
}
