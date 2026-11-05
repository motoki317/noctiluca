package slack

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	literal        = regexp.MustCompile("(?s)```.*?```|`[^`\\n]+`|<[@#!][^>\\n]+>|<https?://[^>\\n]+>")
	bold           = regexp.MustCompile(`\*\*([^\n]+?)\*\*`)
	underscoreBold = regexp.MustCompile(`__([^\n]+?)__`)
	link           = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	heading        = regexp.MustCompile(`(?m)^#{1,6} +(.+)$`)
	list           = regexp.MustCompile(`(?m)^(\s*)[-*+] +`)
)

func escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func mrkdwn(text string) string {
	convert := func(s string) string {
		s = escape(s)
		s = link.ReplaceAllString(s, "<$2|$1>")
		s = bold.ReplaceAllString(s, "*$1*")
		s = underscoreBold.ReplaceAllString(s, "*$1*")
		s = heading.ReplaceAllString(s, "*$1*")
		return list.ReplaceAllString(s, "${1}• ")
	}
	prefix := "\x00literal"
	for strings.Contains(text, prefix) {
		prefix += "x"
	}
	var restore []string
	protected := literal.ReplaceAllStringFunc(text, func(value string) string {
		key := fmt.Sprintf("%s%d\x00", prefix, len(restore)/2)
		restore = append(restore, key, value)
		return key
	})
	return strings.NewReplacer(restore...).Replace(convert(protected))
}
