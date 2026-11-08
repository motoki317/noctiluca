package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	for _, kind := range []string{"slack", "discord", "traq"} {
		for _, block := range []string{"slack", "discord", "traq"} {
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
		{"discord", "token", ""}, {"traq", "access_token", "origin: https://chat.example.org, "},
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
  - name: team
    type: traq
    owner: '00000000-0000-4000-8000-000000000001'
    traq:
      origin: https://chat.example.org
      access_token: {%s: TRAQ_TOKEN}
    conversations:
      direct_messages: {users: []}
      rules:
        - id: '00000000-0000-4000-8000-000000000003'
          users: ['00000000-0000-4000-8000-000000000001']
          groups: ['00000000-0000-4000-8000-000000000004']
`, source, source))
			t.Setenv("TEST_APP_TOKEN", "slack-app-secret")
			t.Setenv("TEST_BOT_TOKEN", "slack-bot-secret")
			for name, value := range map[string]string{"DISCORD_TOKEN": "discord-secret", "TRAQ_TOKEN": "traq-secret"} {
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
				{DiscordToken: "discord-secret"}, {TraQAccessToken: "traq-secret"},
			}, tokens)
			require.Equal(t, "https://chat.example.org", c.Messaging[2].TraQ.Origin)
			for i, id := range []string{"100000000000000003", "00000000-0000-4000-8000-000000000003"} {
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
			require.NotNil(t, c.Messaging[2].Conversations.DirectMessages)
			require.Empty(t, c.Messaging[2].Conversations.DirectMessages.Users)
			if source == "file" {
				require.Equal(t, filepath.Join(filepath.Dir(path), "DISCORD_TOKEN"), c.Messaging[1].Discord.Token.File)
				require.Equal(t, filepath.Join(filepath.Dir(path), "TRAQ_TOKEN"), c.Messaging[2].TraQ.AccessToken.File)
			}
		})
	}
}

func TestNewMessagingTypesValidation(t *testing.T) {
	for _, platform := range []struct{ name, field, settings string }{
		{"discord", "token", ""}, {"traq", "access_token", "origin: https://chat.example.org, "},
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
					if platform.name == "traq" && owner == "owner" {
						owner = "00000000-0000-4000-8000-000000000001"
					}
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

func TestTraQOriginValidation(t *testing.T) {
	t.Setenv("TEST_TRAQ_TOKEN", "traq-secret")
	for _, tc := range []struct {
		origin string
		valid  bool
	}{
		{"https://chat.example.org", true}, {"https://chat.example.org/", true},
		{"http://127.0.0.1:8080", true}, {"http://[::1]:8080", true},
		{"", false}, {"chat.example.org", false}, {"//chat.example.org", false}, {"https://", false},
		{"https://:8080", false}, {"https://chat.example.org:invalid", false}, {"ftp://chat.example.org", false},
		{"https://user:password@chat.example.org", false}, {"https://chat.example.org/api/v3", false},
		{"https://chat.example.org?token=sensitive-token", false}, {"https://chat.example.org?", false},
		{"https://chat.example.org#fragment", false}, {"https://chat.example.org/%zz", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: team
    type: traq
    owner: '00000000-0000-4000-8000-000000000001'
    traq: {origin: %q, access_token: {env: TEST_TRAQ_TOKEN}}
`, tc.origin)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "messaging.team.traq.origin")
				require.NotContains(t, err.Error(), "sensitive-token")
				require.NotContains(t, err.Error(), "password")
			}
		})
	}
}

func TestTraQConfigRequiresUUIDsBeforeResources(t *testing.T) {
	t.Setenv("TEST_TRAQ_TOKEN", "fake-token")
	const valid = "00000000-0000-4000-8000-000000abcdef"
	for _, field := range []string{"owner", "conversations.rules[0].id", "conversations.rules[0].users", "conversations.rules[0].groups", "conversations.direct_messages.users", "conversations.direct_messages.groups"} {
		for _, value := range []string{valid, strings.ToUpper(valid), strings.Replace(valid, "abcdef", "Abcdef", 1), "username", "invalid_id", "00000000-0000-4000-8000-00000000000g", "00000000000040008000000000000001", "{" + valid + "}", "urn:uuid:" + valid} {
			t.Run(field+"/"+value, func(t *testing.T) {
				c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: team
    type: traq
    owner: '%[1]s'
    traq: {origin: https://chat.example.org, access_token: {env: TEST_TRAQ_TOKEN}}
    conversations:
      rules: [{id: '%[1]s', users: ['%[1]s'], groups: ['%[1]s']}]
      direct_messages: {users: ['%[1]s'], groups: ['%[1]s']}
`, valid)))
				require.NoError(t, err)
				entry := &c.Messaging[0]
				switch field {
				case "owner":
					entry.Owner = value
				case "conversations.rules[0].id":
					entry.Conversations.Rules[0].ID = value
				case "conversations.rules[0].users":
					entry.Conversations.Rules[0].Users = []string{value}
				case "conversations.rules[0].groups":
					entry.Conversations.Rules[0].Groups = []string{value}
				case "conversations.direct_messages.users":
					entry.Conversations.DirectMessages.Users = []string{value}
				case "conversations.direct_messages.groups":
					entry.Conversations.DirectMessages.Groups = []string{value}
				}
				if value == valid {
					_, _, err = c.validateRun()
					require.NoError(t, err)
				} else {
					_, err = newApplication(t.Context(), c, nil)
					require.Error(t, err)
					if field == "conversations.rules[0].id" && strings.Contains(value, ":") {
						require.ErrorContains(t, err, "unique channel IDs")
					} else {
						require.ErrorContains(t, err, "messaging.team."+field)
						require.ErrorContains(t, err, "UUIDs in lowercase canonical form")
					}
				}
				require.NoFileExists(t, c.Database.Path)
				require.NoDirExists(t, c.Memory.Dir)
				require.NoDirExists(t, c.Model.Workdir)
			})
		}
	}
}

func TestDiscordApplicationConstruction(t *testing.T) {
	t.Setenv("TEST_DISCORD_TOKEN", "fake-token")
	c, err := loadConfig(writeConfig(t, `messaging:
  - name: chat
    type: discord
    owner: "200"
    discord: {token: {env: TEST_DISCORD_TOKEN}}
    conversations:
      rules: [{id: "400", groups: ["700"]}]
      direct_messages: {groups: ["701"]}
heartbeat: {enabled: false}
`))
	require.NoError(t, err)
	app, err := newApplication(t.Context(), c, nil)
	require.NoError(t, err)
	require.NoError(t, app.Shutdown(t.Context()))
	require.FileExists(t, c.Database.Path)
}
