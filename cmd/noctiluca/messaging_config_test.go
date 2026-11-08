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
	require.Equal(t, []messagingTokens{
		{Slack: slack.Config{AppToken: "support-app-secret", BotToken: "support-bot-secret", UserToken: "support-user-secret"}},
		{Slack: slack.Config{AppToken: "slack-app-secret", BotToken: "slack-bot-secret"}},
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

func TestMismatchedMessagingSettingsRejected(t *testing.T) {
	for _, kind := range []string{"slack", "discord"} {
		for _, block := range []string{"slack", "discord"} {
			for _, value := range []string{"{}", "null", "{token: sensitive-token}"} {
				if kind == block {
					continue
				}
				t.Run(kind+"/"+block+"/"+value, func(t *testing.T) {
					_, err := loadConfig(writeConfig(t, fmt.Sprintf("messaging: [{name: chat, type: %s, owner: owner, %s: %s}]", kind, block, value)))
					require.ErrorContains(t, err, "has "+block+" settings but a different type")
					require.NotContains(t, err.Error(), "sensitive-token")
				})
			}
		}
	}
}

func TestEveryInstanceTokenFileProtected(t *testing.T) {
	for _, tc := range []struct{ platform, field, settings string }{
		{"slack", "app_token", ""}, {"slack", "bot_token", ""}, {"slack", "user_token", ""},
		{"discord", "token", ""},
	} {
		t.Run(tc.platform+"."+tc.field, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - {name: slack, type: slack, owner: U_OWNER}
  - name: support
    type: %s
    owner: U_SUPPORT
    %s: {%s%s: {file: work/token}}
model: {workdir: work}
`, tc.platform, tc.platform, tc.settings, tc.field)))
			require.NoError(t, err)
			_, err = newApplication(t.Context(), c, nil)
			require.ErrorContains(t, err, "model.workdir")
			require.NoFileExists(t, c.Database.Path)
			require.NoDirExists(t, c.Model.Workdir)
		})
	}
}

func TestMixedMessagingTypesConfig(t *testing.T) {
	for _, source := range []string{"env", "file"} {
		t.Run(source, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(`messaging:
  - name: office
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
  - name: community
    type: discord
    owner: '100000000000000001'
    discord:
      token: {%s: DISCORD_TOKEN}
    conversations:
      direct_messages: {users: ['100000000000000002']}
      rules:
        - id: '100000000000000003'
          users: ['100000000000000001']
          groups: ['100000000000000004']
          allow_bots: true
          require_mention: true
`, source))
			t.Setenv("TEST_APP_TOKEN", "slack-app-secret")
			t.Setenv("TEST_BOT_TOKEN", "slack-bot-secret")
			for name, value := range map[string]string{"DISCORD_TOKEN": "discord-secret"} {
				if source == "env" {
					t.Setenv(name, value)
				} else {
					require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(value+"\n"), 0600))
				}
			}
			c, err := loadConfig(path)
			require.NoError(t, err)
			tokens, _, err := c.validateRun()
			require.NoError(t, err)
			require.Equal(t, []messagingTokens{
				{Slack: slack.Config{AppToken: "slack-app-secret", BotToken: "slack-bot-secret"}},
				{DiscordToken: "discord-secret"},
			}, tokens)
			for i, id := range []string{"100000000000000003"} {
				entry := c.Messaging[i+1]
				require.Equal(t, time.Hour, entry.groupRefreshInterval())
				require.Len(t, entry.Conversations.Rules, 1)
				rule := entry.Conversations.Rules[0]
				require.Equal(t, id, rule.ID)
				require.Equal(t, []string{entry.Owner}, rule.Users)
				require.Equal(t, []string{id[:len(id)-1] + "4"}, rule.Groups)
				require.Equal(t, i == 0, rule.AllowBots)
				require.Equal(t, i == 0, rule.RequireMention)
			}
			require.Equal(t, []string{"100000000000000002"}, c.Messaging[1].Conversations.DirectMessages.Users)
			if source == "file" {
				require.Equal(t, filepath.Join(filepath.Dir(path), "DISCORD_TOKEN"), c.Messaging[1].Discord.Token.File)
			}
		})
	}
}

func TestNewMessagingTypesValidation(t *testing.T) {
	for _, platform := range []struct{ name, field, settings string }{
		{"discord", "token", ""},
	} {
		t.Run(platform.name, func(t *testing.T) {
			t.Setenv("TEST_NEW_TOKEN", "sensitive-token")
			t.Setenv("TEST_EMPTY_TOKEN", "")
			t.Setenv("TEST_BAD_TOKEN", "sensitive token")
			for _, tc := range []struct{ name, owner, token, rules, want string }{
				{"missing owner", "''", "{env: TEST_NEW_TOKEN}", "[]", "chat.owner"},
				{"missing token", "owner", "{}", "[]", platform.field},
				{"empty token", "owner", "{env: TEST_EMPTY_TOKEN}", "[]", platform.field},
				{"missing token file", "owner", "{file: missing-token}", "[]", platform.field},
				{"conflicting sources", "owner", "{env: TEST_NEW_TOKEN, file: token}", "[]", "choose env or file"},
				{"whitespace token", "owner", "{env: TEST_BAD_TOKEN}", "[]", "whitespace"},
				{"duplicate channels", "owner", "{env: TEST_NEW_TOKEN}", "[{id: channel}, {id: channel}]", "unique channel IDs"},
				{"empty channel", "owner", "{env: TEST_NEW_TOKEN}", "[{id: ''}]", "unique channel IDs"},
				{"invalid channel", "owner", "{env: TEST_NEW_TOKEN}", "[{id: 'channel:thread'}]", "unique channel IDs"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					owner := tc.owner
					c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: chat
    type: %s
    owner: %s
    %s: {%s%s: %s}
    conversations: {rules: %s}
`, platform.name, owner, platform.name, platform.settings, platform.field, tc.token, tc.rules)))
					require.NoError(t, err)
					_, err = newApplication(t.Context(), c, nil)
					require.ErrorContains(t, err, tc.want)
					require.NotContains(t, err.Error(), "sensitive")
					require.NoFileExists(t, c.Database.Path)
					require.NoDirExists(t, c.Memory.Dir)
					require.NoDirExists(t, c.Model.Workdir)
				})
			}
			_, err := loadConfig(writeConfig(t, fmt.Sprintf("messaging: [{name: chat, type: %s, owner: owner, %s: {%s: sensitive-token}}]", platform.name, platform.name, platform.field)))
			require.ErrorContains(t, err, "invalid config")
			require.NotContains(t, err.Error(), "sensitive-token")
		})
	}
}

func TestUnimplementedMessagingAdaptersBeforeResources(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "slack-app-secret")
	t.Setenv("TEST_BOT_TOKEN", "slack-bot-secret")
	t.Setenv("TEST_NEW_TOKEN", "new-secret")
	for _, platform := range []struct{ name, settings string }{
		{"discord", "token: {env: TEST_NEW_TOKEN}"},
	} {
		t.Run(platform.name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: office
    type: slack
    owner: U_OWNER
    slack: {app_token: {env: TEST_APP_TOKEN}, bot_token: {env: TEST_BOT_TOKEN}}
  - name: chat
    type: %s
    owner: '100000000000000001'
    %s: {%s}
`, platform.name, platform.name, platform.settings)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			require.NoError(t, err)
			_, err = newApplication(t.Context(), c, nil)
			require.EqualError(t, err, fmt.Sprintf("messaging.chat: %s adapter is not implemented yet", platform.name))
			require.NoFileExists(t, c.Database.Path)
			require.NoDirExists(t, c.Memory.Dir)
			require.NoDirExists(t, c.Model.Workdir)
		})
	}
}
