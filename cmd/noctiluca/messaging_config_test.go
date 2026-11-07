package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
)

func TestMultipleMessagingConfig(t *testing.T) {
	path := writeConfig(t, `messaging:
  - name: support_2
    type: slack
    owner: U_SUPPORT
    slack:
      app_token: {file: support-app}
      bot_token: {env: SUPPORT_BOT}
      user_token: {file: support-user}
      group_refresh_interval: 15m
    conversations:
      direct_messages: {users: [U_SUPPORT, U_OTHER]}
      rules: [{id: C_SHARED, users: [U_SUPPORT], groups: [S_SHARED]}]
  - name: slack
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {file: slack-bot}
    conversations:
      rules: [{id: C_SHARED, users: [U_OWNER], groups: [S_SHARED]}]
`)
	t.Setenv("SUPPORT_BOT", "support-bot-secret")
	t.Setenv("TEST_APP_TOKEN", "slack-app-secret")
	for file, secret := range map[string]string{"support-app": "support-app-secret", "support-user": "support-user-secret", "slack-bot": "slack-bot-secret"} {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), file), []byte(secret+"\n"), 0600))
	}
	c, err := loadConfig(path)
	require.NoError(t, err)
	tokens, _, err := c.validateRun()
	require.NoError(t, err)
	require.Equal(t, []slack.Config{
		{AppToken: "support-app-secret", BotToken: "support-bot-secret", UserToken: "support-user-secret"},
		{AppToken: "slack-app-secret", BotToken: "slack-bot-secret"},
	}, tokens)
	require.Len(t, c.Messaging, 2)
	require.Equal(t, "support_2", c.Messaging[0].Name)
	require.Equal(t, "slack", c.Messaging[1].Name)
	require.Equal(t, "U_SUPPORT", c.Messaging[0].Owner)
	require.Equal(t, "U_OWNER", c.Messaging[1].Owner)
	require.Equal(t, 15*time.Minute, c.Messaging[0].groupRefreshInterval())
	require.Equal(t, time.Hour, c.Messaging[1].groupRefreshInterval())
	require.Equal(t, []string{"U_SUPPORT", "U_OTHER"}, c.Messaging[0].Conversations.DirectMessages.Users)
	require.Nil(t, c.Messaging[1].Conversations.DirectMessages)
	for i, owner := range []string{"U_SUPPORT", "U_OWNER"} {
		require.Equal(t, []string{owner}, c.Messaging[i].Conversations.Rules[0].Users)
	}
}

func TestInvalidMessagingConfigBeforeResources(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"empty", "messaging: []", "messaging requires at least one entry"},
		{"missing name", "messaging: [{type: slack, owner: U_OWNER}]", "names"},
		{"duplicate", "messaging: [{name: slack, type: slack, owner: U_OWNER}, {name: slack, type: slack, owner: U_OTHER}]", "unique"},
		{"missing type", "messaging: [{name: slack, owner: U_OWNER}]", "type"},
		{"unsupported type", "messaging: [{name: slack, type: other, owner: U_OWNER}]", "type"},
		{"missing owner", "messaging: [{name: slack, type: slack}]", "owner"},
		{"second owner missing", "messaging: [{name: slack, type: slack, owner: U_OWNER}, {name: second, type: slack}]", "second.owner"},
		{"zero interval", "messaging: [{name: slack, type: slack, owner: U_OWNER, slack: {group_refresh_interval: 0s}}]", "positive"},
		{"negative interval", "messaging: [{name: slack, type: slack, owner: U_OWNER, slack: {group_refresh_interval: -1s}}]", "positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, tc.yaml))
			require.NoError(t, err)
			_, err = newApplication(t.Context(), c, nil)
			require.ErrorContains(t, err, tc.want)
			require.NoFileExists(t, c.Database.Path)
			require.NoDirExists(t, c.Memory.Dir)
			require.NoDirExists(t, c.Model.Workdir)
		})
	}
	for _, name := range []string{"Slack", "1slack", "slack-team", "slack:team", "slack team", "_slack", "slack.team", "équipe"} {
		t.Run(name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf("messaging: [{name: %q, type: slack, owner: U_OWNER}]", name)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			require.ErrorContains(t, err, "names")
		})
	}
}

func TestLegacyMessagingConfigRejected(t *testing.T) {
	for _, legacy := range []string{"owner: U_OWNER", "owner: ''", "owner: null", "slack: null", "conversations: null", "slack: {}", "conversations: {}", "slack: {app_token: {env: TEST_APP_TOKEN}}", "conversations: {rules: [{id: C_ROOM}]}"} {
		for _, prefix := range []string{"", "messaging: [{name: slack, type: slack, owner: U_OWNER}]\n"} {
			_, err := loadConfig(writeConfig(t, prefix+legacy+"\n"))
			require.ErrorContains(t, err, "invalid config", legacy)
		}
	}
}

func TestEveryInstanceTokenFileProtected(t *testing.T) {
	for _, field := range []string{"app_token", "bot_token", "user_token"} {
		t.Run(field, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - {name: slack, type: slack, owner: U_OWNER}
  - name: support
    type: slack
    owner: U_SUPPORT
    slack: {%s: {file: work/token}}
model: {workdir: work}
`, field)))
			require.NoError(t, err)
			_, err = newApplication(t.Context(), c, nil)
			require.ErrorContains(t, err, "model.workdir")
			require.NoFileExists(t, c.Database.Path)
			require.NoDirExists(t, c.Model.Workdir)
		})
	}
}
