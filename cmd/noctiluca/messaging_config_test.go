package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
)

func TestLegacyMessagingConfigRejected(t *testing.T) {
	for _, legacy := range []string{"owner: U_OWNER", "slack: null", "conversations: {}"} {
		_, err := loadConfig(writeConfig(t, legacy+"\n"))
		require.ErrorContains(t, err, "invalid config", legacy)
	}
}

func TestMismatchedMessagingSettingsRejected(t *testing.T) {
	for _, value := range []string{"null", "{token: sensitive-token}"} {
		_, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: owner, discord: "+value+"}]"))
		require.ErrorContains(t, err, "has discord settings but a different type")
		require.NotContains(t, err.Error(), "sensitive-token")
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
			_, _, err = c.validateRun()
			require.ErrorContains(t, err, "model.workdir")
		})
	}
}

func TestMixedMessagingTypesConfig(t *testing.T) {
	path := writeConfig(t, `messaging:
  - name: office
    type: slack
    owner: U_OWNER
    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
      user_token: {file: SLACK_USER_TOKEN}
  - name: community
    type: discord
    owner: '100000000000000001'
    discord: {token: {file: DISCORD_TOKEN}}
  - name: team
    type: traq
    owner: '00000000-0000-4000-8000-000000000001'
    traq:
      origin: https://chat.example.org
      access_token: {file: TRAQ_TOKEN}
`)
	t.Setenv("TEST_APP_TOKEN", "slack-app-secret")
	t.Setenv("TEST_BOT_TOKEN", "slack-bot-secret")
	for name, value := range map[string]string{
		"SLACK_USER_TOKEN": "slack-user-secret", "DISCORD_TOKEN": "discord-secret", "TRAQ_TOKEN": "traq-secret",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(value+"\n"), 0600))
	}
	c, err := loadConfig(path)
	require.NoError(t, err)
	tokens, _, err := c.validateRun()
	require.NoError(t, err)
	require.Equal(t, []messagingTokens{
		{Slack: slack.Config{AppToken: "slack-app-secret", BotToken: "slack-bot-secret", UserToken: "slack-user-secret"}},
		{DiscordToken: "discord-secret"}, {TraQAccessToken: "traq-secret"},
	}, tokens)
}

func TestNewMessagingTypesValidation(t *testing.T) {
	for _, platform := range []struct{ name, field, settings, owner string }{
		{"discord", "token", "", "200"},
		{"traq", "access_token", "origin: https://chat.example.org, ", "00000000-0000-4000-8000-000000000001"},
	} {
		t.Run(platform.name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(
				"messaging: [{name: chat, type: %s, owner: %q, %s: {%s%s: {}}}]",
				platform.name, platform.owner, platform.name, platform.settings, platform.field)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			require.ErrorContains(t, err, "messaging.chat."+platform.name+"."+platform.field)

			_, err = loadConfig(writeConfig(t, fmt.Sprintf(
				"messaging: [{name: chat, type: %s, owner: %q, %s: {%s: sensitive-token}}]",
				platform.name, platform.owner, platform.name, platform.field)))
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
		{"https://chat.example.org", true},
		{"https://user:password@chat.example.org", false},
		{"https://chat.example.org?token=sensitive-token", false},
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

func TestTraQConfigRequiresCanonicalUUIDs(t *testing.T) {
	t.Setenv("TEST_TRAQ_TOKEN", "fake-token")
	const valid = "00000000-0000-4000-8000-000000abcdef"
	for _, field := range []string{"owner", "conversations[0].id", "conversations[0].trust.users", "conversations[0].trust.groups", "conversations[0].trust.bots", "trust.users", "trust.groups", "trust.bots"} {
		values := []string{strings.ToUpper(valid)}
		if field == "owner" {
			values = append(values, "username", strings.ReplaceAll(valid, "-", ""), "{"+valid+"}", "urn:uuid:"+valid)
		}
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: team
    type: traq
    owner: '%[1]s'
    traq: {origin: https://chat.example.org, access_token: {env: TEST_TRAQ_TOKEN}}
    trust: {users: ['%[1]s'], groups: ['%[1]s'], bots: ['%[1]s']}
    conversations:
      - {id: '%[1]s', trust: {users: ['%[1]s'], groups: ['%[1]s'], bots: ['%[1]s']}}
`, valid)))
				require.NoError(t, err)
				_, _, err = c.validateRun()
				require.NoError(t, err)
				entry := &c.Messaging[0]
				switch field {
				case "owner":
					entry.Owner = value
				case "conversations[0].id":
					entry.Conversations[0].ID = value
				case "conversations[0].trust.users":
					entry.Conversations[0].Trust.Users = []string{value}
				case "conversations[0].trust.groups":
					entry.Conversations[0].Trust.Groups = []string{value}
				case "conversations[0].trust.bots":
					entry.Conversations[0].Trust.Bots = []string{value}
				case "trust.users":
					entry.Trust.Users = []string{value}
				case "trust.groups":
					entry.Trust.Groups = []string{value}
				case "trust.bots":
					entry.Trust.Bots = []string{value}
				}
				_, _, err = c.validateRun()
				require.ErrorContains(t, err, "messaging.team."+field)
				require.ErrorContains(t, err, "UUIDs in lowercase canonical form")
			})
		}
	}
}

func TestDiscordOverrideIDs(t *testing.T) {
	t.Setenv("TEST_DISCORD_TOKEN", "fake-token")
	for _, id := range []string{"400", "general", "0", "0400"} {
		t.Run(id, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, fmt.Sprintf(`messaging:
  - name: chat
    type: discord
    owner: '200'
    discord: {token: {env: TEST_DISCORD_TOKEN}}
    conversations: [{id: '300'}, {id: %q}]
`, id)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			if id == "400" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "messaging.chat.conversations[1].id must be a Discord numeric ID")
			}
		})
	}
}

func TestApplicationConstruction(t *testing.T) {
	t.Setenv("TEST_TOKEN", "fake-token")
	for _, tc := range []struct{ name, entry string }{
		{"discord", `
  - name: chat
    type: discord
    owner: "200"
    discord: {token: {env: TEST_TOKEN}}
    trust: {groups: ["701"]}
    conversations: [{id: "400", trust: {groups: ["700"]}}]
`},
		{"traq", `
  - name: team
    type: traq
    owner: '00000000-0000-4000-8000-000000000001'
    traq: {origin: https://chat.example.org, access_token: {env: TEST_TOKEN}}
    trust: {groups: ['00000000-0000-4000-8000-000000000002']}
    conversations: [{id: '00000000-0000-4000-8000-000000000003', require_mention: true}]
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, "messaging:"+tc.entry+"heartbeat: {enabled: false}\n"))
			require.NoError(t, err)
			app, err := newApplication(t.Context(), c, nil)
			require.NoError(t, err)
			require.NoError(t, app.Shutdown(t.Context()))
			require.FileExists(t, c.Database.Path)
		})
	}
}

func TestDiscordGroupsRequiredByEntryOrOverride(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entry     domain.AcceptRule
		overrides map[domain.ConversationRef]domain.AcceptRule
		want      bool
	}{
		{"users and bots", domain.AcceptRule{Users: []string{"300"}, Bots: []string{"400"}}, nil, false},
		{"entry groups", domain.AcceptRule{Groups: []string{"500"}}, nil, true},
		{"override-only groups", domain.AcceptRule{}, map[domain.ConversationRef]domain.AcceptRule{"chat:600:": {Groups: []string{"500"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, needsGroups(tc.entry, tc.overrides))
		})
	}
}

func TestLegacyConversationConfigNamesReplacement(t *testing.T) {
	_, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: U_OWNER, conversations: {}}]"))
	require.ErrorContains(t, err, "conversations list")
	for _, fields := range []string{
		"allow_bots: true", "trust: {allow_bots: false}",
		"conversations: [{id: C_ROOM, allow_bots: true}]", "conversations: [{id: C_ROOM, trust: {allow_bots: null}}]",
	} {
		_, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: U_OWNER, "+fields+"}]"))
		require.ErrorContains(t, err, "allow_bots")
		require.ErrorContains(t, err, "trust.bots")
	}
}

func TestIgnoreOverrideRejectsOtherKeys(t *testing.T) {
	_, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: U_OWNER, conversations: [{id: C_ROOM, ignore: true, require_mention: false}]}]"))
	require.ErrorContains(t, err, "ignore: true permits only id and ignore")
	c, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: U_OWNER, conversations: [{id: C_ROOM, ignore: true}]}]"))
	require.NoError(t, err)
	require.True(t, c.Messaging[0].Conversations[0].Ignore)
}

func TestNullOverrideTrustRejected(t *testing.T) {
	_, err := loadConfig(writeConfig(t, `messaging:
  - name: chat
    type: slack
    owner: U_OWNER
    trust: {users: [U_MEMBER]}
    conversations:
      - id: CROOM
        trust: null
`))
	require.ErrorContains(t, err, "conversations[0].trust cannot be null")
}

func TestIgnoredOverrideRequiresID(t *testing.T) {
	c, err := loadConfig(writeConfig(t, "messaging: [{name: chat, type: slack, owner: U_OWNER, conversations: [{ignore: true}]}]"))
	require.NoError(t, err)
	_, _, err = c.validateRun()
	require.ErrorContains(t, err, "conversation overrides require unique channel IDs")
}

func TestSlackOverrideIDs(t *testing.T) {
	t.Setenv("TEST_APP_TOKEN", "app-token")
	t.Setenv("TEST_BOT_TOKEN", "bot-token")
	for _, id := range []string{"C012ABC", "U012ABC", "C_ROOM"} {
		t.Run(id, func(t *testing.T) {
			c, err := loadConfig(writeConfig(t, slackEntry+fmt.Sprintf(`    slack:
      app_token: {env: TEST_APP_TOKEN}
      bot_token: {env: TEST_BOT_TOKEN}
    conversations: [{id: %q}]
`, id)))
			require.NoError(t, err)
			_, _, err = c.validateRun()
			if id == "C012ABC" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "messaging.slack.conversations[0].id must be a Slack channel ID")
			}
		})
	}
}
