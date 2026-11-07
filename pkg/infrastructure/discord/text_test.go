package discord

import (
	"strings"
	"testing"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/stretchr/testify/assert"
)

func TestThreadName(t *testing.T) {
	displayName, emptyName := "Display Name", ""
	mentions := []sdk.User{
		{ID: 100, Username: "bot"},
		{ID: 200, Username: "reader", GlobalName: &displayName},
		{ID: 201, Username: "helper"},
		{ID: 202, Username: "fallback", GlobalName: &emptyName},
	}
	for _, tc := range []struct {
		name, text, want string
	}{
		{"plain", "Original request", "Original request"},
		{"bot mention", "<@100> run date", "run date"},
		{"bot nickname mention", "<@!100> run date <@100>", "run date"},
		{"known users", "Ask <@200> and <@!201> with <@202>", "Ask @Display Name and @helper with @fallback"},
		{"unknown users", "Ask <@999> and <@!998>", "Ask and"},
		{"roles and channels", "Ask <@&200> in <#201> now", "Ask in now"},
		{"custom emoji", "<:wave:300> <a:party_time:301> 🙂", ":wave: :party_time: 🙂"},
		{"whitespace", " \t<@100>  Ask\t<@200>\u3000<@&300>  now  ", "Ask @Display Name now"},
		{"first line", "<@100> First\r\nSecond", "First"},
		{"leading blank lines", "\n\t\n <@100> First\nSecond", "First"},
		{"empty", "", "Reply"},
		{"only whitespace", " \t\n ", "Reply"},
		{"only markup", "<@100> <@!100> <@999> <@&200> <#201>", "Reply"},
		{"empty first line after cleanup", "<@100>\nSecond", "Reply"},
		{"unrelated markup", "<https://example.org> **keep** <@invalid>", "<https://example.org> **keep** <@invalid>"},
		{"at limit", strings.Repeat("x", 100), strings.Repeat("x", 100)},
		{"over limit", strings.Repeat("x", 101), strings.Repeat("x", 100)},
		{"trim cut", strings.Repeat("x", 99) + " next", strings.Repeat("x", 99)},
		{"unicode limit", strings.Repeat("界", 99) + "🙂", strings.Repeat("界", 99)},
		{"emoji limit", strings.Repeat("🙂", 51), strings.Repeat("🙂", 50)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, threadName(sdk.Message{Content: tc.text, Mentions: mentions}, 100))
		})
	}
}
