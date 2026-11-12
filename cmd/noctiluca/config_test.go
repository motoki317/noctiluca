package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
)

const slackEntry = "messaging:\n  - name: slack\n    type: slack\n    owner: U_OWNER\n"

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
model: {executable: ./claude}
memory: {dir: memories}
database: {path: history.db}
toolhub: {listen: '127.0.0.1:12345'}
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
	require.Equal(t, 12345, port)
	require.Equal(t, filepath.Join(filepath.Dir(path), "history.db"), c.Database.Path)
	require.Equal(t, filepath.Join(filepath.Dir(path), "memories"), c.Memory.Dir)
	require.Equal(t, filepath.Join(filepath.Dir(path), "claude"), c.Model.Executable)
	require.Equal(t, filepath.Join(filepath.Dir(path), "work"), c.Cron[0].Workdir)
}

func TestSecretReferencesDoNotExposeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("secret-value\n"), 0600))
	t.Setenv("TEST_TOKEN", "secret-value")
	t.Setenv("EMPTY_TOKEN", "")
	t.Setenv("WHITESPACE_TOKEN", "secret-value with spaces")
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
		{ref: secretRef{Env: "WHITESPACE_TOKEN"}, fails: true},
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
		{slackEntry, "slack.app_token"},
		{slackEntry + "toolhub: {listen: '0.0.0.0:1234'}\n", "toolhub.listen"},
		{slackEntry + "heartbeat: {interval: -1s}\n", "interval"},
		{slackEntry + "    conversations: [{id: CCHANNEL}, {id: CCHANNEL}]\n", "unique"},
		{"messaging: [{type: slack, owner: U_OWNER}]", "names"},
		{"messaging: [{name: slack, type: slack, owner: U_OWNER}, {name: slack, type: slack, owner: U_OTHER}]", "unique"},
		{"messaging: [{name: slack, type: other, owner: U_OWNER}]", "type"},
		{slackEntry + "    slack: {group_refresh_interval: 0s}\n", "positive"},
		{"messaging: [{name: 'slack:team', type: slack, owner: U_OWNER}]", "names"},
		{"messaging: [{name: Slack, type: slack, owner: U_OWNER}]", "names"},
	} {
		c, err := loadConfig(writeConfig(t, tc.yaml))
		require.NoError(t, err)
		_, err = newApplication(t.Context(), c, nil)
		require.ErrorContains(t, err, tc.want)
		require.NoFileExists(t, c.Database.Path)
		require.NoDirExists(t, c.Memory.Dir)
		require.NoDirExists(t, c.Model.Workdir)
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := loadConfig("../../config.example.yaml")
	require.NoError(t, err)
	t.Setenv("SLACK_APP_TOKEN", "app-example")
	t.Setenv("SLACK_BOT_TOKEN", "bot-example")
	_, _, err = c.validateRun()
	require.NoError(t, err)
}

func TestEntryAndConversationTrustRules(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         domain.AcceptRule
		wantErr      bool
	}{
		{"omitted", "", domain.AcceptRule{AnySender: true}, false},
		{"empty", "trust: {}", domain.AcceptRule{}, false},
		{"null", "trust: null", domain.AcceptRule{}, true},
		{"allowlist", "trust: {users: [U_MEMBER], bots: [U_ALERT]}", domain.AcceptRule{Users: []string{"U_MEMBER"}, Bots: []string{"U_ALERT"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, "messaging:\n  - name: chat\n    type: slack\n    owner: U_OWNER\n    "+tc.fields+`
    conversations:
      - id: CINHERIT
        require_mention: true
      - id: CEMPTY
        trust: {}
      - id: CLISTED
        trust: {users: [U_OTHER]}
`))
			if tc.wantErr {
				require.ErrorContains(t, err, "trust cannot be null")
				return
			}
			require.NoError(t, err)
			entry := c.Messaging[0]
			require.Equal(t, tc.want, entry.Trust.rule())
			inherited := tc.want
			inherited.RequireMention = true
			require.Equal(t, inherited, entry.Conversations[0].rule(entry.Trust))
			require.Equal(t, domain.AcceptRule{}, entry.Conversations[1].rule(entry.Trust))
			require.Equal(t, domain.AcceptRule{Users: []string{"U_OTHER"}}, entry.Conversations[2].rule(entry.Trust))
		})
	}
}

func TestDisabledReservedSchedulesAreRemoved(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	c, err := loadConfig(writeConfig(t, slackEntry+"    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\nheartbeat: {enabled: false}\n"))
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
		c, err := loadConfig(writeConfig(t, slackEntry+"cron:\n  - {name: "+name+", cron: '0 4 * * *', tz: UTC, prompt: run}\n"))
		require.NoError(t, err)
		_, _, err = c.validateRun()
		require.ErrorContains(t, err, "reserved cron names")
	}
}

func TestModelWorkdirDefault(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "xapp-fake")
	t.Setenv("TEST_BOT_TOKEN", "xoxb-fake")
	c, err := loadConfig(writeConfig(t, slackEntry+"    slack:\n      app_token: {env: TEST_APP_TOKEN}\n      bot_token: {env: TEST_BOT_TOKEN}\n"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(filepath.Dir(c.Database.Path), "work"), c.Model.Workdir)
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
			path := writeConfig(t, slackEntry+body)
			require.NoError(t, os.Symlink(filepath.Dir(path), filepath.Join(filepath.Dir(path), "alias")))
			c, err := loadConfig(path)
			require.NoError(t, err)
			_, _, err = c.validateRun()
			require.ErrorContains(t, err, "model.workdir")
		})
	}
}
func TestCronConfigRequiresFiveFields(t *testing.T) {
	c, err := loadConfig(writeConfig(t, slackEntry+"cron: [{name: test, cron: '@daily', tz: UTC, prompt: run}]\n"))
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "five fields")
}

func TestWorkdirRejectsSecretSymlinkInside(t *testing.T) {
	path := writeConfig(t, slackEntry+"    slack: {bot_token: {file: work/token}}\nmodel: {workdir: work}\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "work"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("token"), 0600))
	require.NoError(t, os.Symlink("../secret", filepath.Join(dir, "work", "token")))
	c, err := loadConfig(path)
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "model.workdir")
}

func TestWorkdirRejectsDanglingSymlinkWithParentTraversal(t *testing.T) {
	path := writeConfig(t, slackEntry+"model: {workdir: other/work}\ndatabase: {path: database-link}\n")
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "other", "child"), 0700))
	require.NoError(t, os.Symlink("other/child", filepath.Join(dir, "alias")))
	require.NoError(t, os.Symlink("alias/../work/db", filepath.Join(dir, "database-link")))
	c, err := loadConfig(path)
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "model.workdir")
}
