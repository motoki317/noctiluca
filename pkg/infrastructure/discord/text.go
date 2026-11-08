package discord

import (
	"errors"
	"strings"
	"unicode/utf8"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

const messageLimit = 2000

func threadName(root sdk.Message, self snowflake.ID) string {
	names := make(map[string]string, len(root.Mentions))
	for _, user := range root.Mentions {
		if user.ID != self {
			name := user.EffectiveName()
			if name == "" {
				name = user.Username
			}
			names[user.ID.String()] = "@" + name
		}
	}
	name, _, _ := strings.Cut(strings.TrimSpace(root.Content), "\n")
	name = sdk.MentionTypeRole.ReplaceAllString(name, "")
	name = sdk.MentionTypeChannel.ReplaceAllString(name, "")
	name = sdk.MentionTypeEmoji.ReplaceAllString(name, ":${1}:")
	name = sdk.MentionTypeUser.ReplaceAllStringFunc(name, func(mention string) string {
		return names[sdk.MentionTypeUser.FindStringSubmatch(mention)[1]]
	})
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "Reply"
	}
	return strings.TrimSpace(name[:textPrefix(name, 100)])
}

func textUnits(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

func textPrefix(text string, limit int) int {
	n := 0
	for i, r := range text {
		n++
		if r > 0xffff {
			n++
		}
		if n > limit {
			return i
		}
	}
	return len(text)
}

type codeFence struct{ marker, header string }

func (f codeFence) closing() string {
	if f.marker == "" {
		return ""
	}
	return "\n" + f.marker
}
func (f codeFence) opening() string {
	if f.marker == "" {
		return ""
	}
	return f.header + "\n"
}

func fenceAfter(f codeFence, line string) codeFence {
	trim := strings.TrimLeft(line, " ")
	if len(line)-len(trim) > 3 || len(trim) < 3 {
		return f
	}
	char := trim[0]
	if char != '`' && char != '~' {
		return f
	}
	n := 0
	for n < len(trim) && trim[n] == char {
		n++
	}
	if n < 3 {
		return f
	}
	rest := strings.TrimSpace(trim[n:])
	if f.marker != "" {
		if char == f.marker[0] && n >= len(f.marker) && rest == "" {
			return codeFence{}
		}
		return f
	}
	if char == '`' && strings.Contains(rest, "`") {
		return f
	}
	return codeFence{marker: trim[:n], header: strings.TrimRight(line, "\r\n")}
}

func splitText(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("message text must be valid UTF-8")
	}
	if text == "" {
		return nil, nil
	}
	var chunks []string
	var current strings.Builder
	fence := codeFence{}
	hasContent := false
	flush := func() {
		chunks = append(chunks, current.String()+fence.closing())
		current.Reset()
		current.WriteString(fence.opening())
		hasContent = false
	}
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		// Prefer a paragraph boundary when the next paragraph fits in a fresh message.
		if i > 0 && strings.TrimSpace(lines[i-1]) == "" && fence.marker == "" && hasContent {
			paragraph := 0
			for _, next := range lines[i:] {
				paragraph += textUnits(next)
				if strings.TrimSpace(next) == "" {
					break
				}
			}
			if paragraph <= messageLimit && textUnits(current.String())+paragraph > messageLimit {
				flush()
			}
		}
		nextFence := fenceAfter(fence, line)
		if textUnits(current.String())+textUnits(line)+textUnits(nextFence.closing()) > messageLimit && hasContent && current.String() != fence.opening() {
			flush()
		}
		if textUnits(current.String())+textUnits(line)+textUnits(nextFence.closing()) <= messageLimit {
			current.WriteString(line)
			hasContent = true
			fence = nextFence
			continue
		}
		if nextFence != fence {
			return nil, errors.New("code fence header exceeds Discord's 2000-character message limit")
		}
		for line != "" {
			budget := messageLimit - textUnits(current.String()) - textUnits(fence.closing())
			n := textPrefix(line, budget)
			// Reopening a block must not turn literal inline backticks into a closing fence.
			for n > 0 && n < len(line) && (fenceAfter(fence, line[:n]) != fence || fenceAfter(fence, line[n:]) != fence) {
				_, size := utf8.DecodeLastRuneInString(line[:n])
				n -= size
			}
			if n == 0 {
				return nil, errors.New("code fence leaves no room within Discord's 2000-character message limit")
			}
			current.WriteString(line[:n])
			hasContent = true
			line = line[n:]
			if line != "" {
				flush()
			}
		}
	}
	if hasContent {
		flush()
	}
	return chunks, nil
}
