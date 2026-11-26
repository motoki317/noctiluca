package slack

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	sdk "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (a *adapter) inbound(ctx context.Context, data any) (domain.InboundEvent, bool) {
	var event domain.InboundEvent
	var channel, thread, itemAuthor string
	var mentionsMe bool
	switch e := data.(type) {
	case *slackevents.MessageEvent:
		kind := domain.InboundEventMessage
		switch e.SubType {
		case "", "bot_message", "file_share", "me_message", "thread_broadcast":
		case "message_changed", "message_deleted":
			kind = domain.InboundEventMessageEdited
		default:
			return event, false
		}
		m := e.Message
		deleted := e.SubType == "message_deleted"
		if deleted {
			m = e.PreviousMessage
			if m == nil {
				return event, false
			}
		}
		if m == nil {
			m = &sdk.Msg{
				SubType: e.SubType, User: e.User, BotID: e.BotID, Username: e.Username,
				Text: e.Text, ThreadTimestamp: e.ThreadTimeStamp, Timestamp: e.TimeStamp,
			}
		}
		channel, thread = e.Channel, m.ThreadTimestamp
		if a.isSelf(m.User, m.BotID) {
			if thread == "" {
				thread = m.Timestamp
			}
			if channel != "" && thread != "" {
				a.joinedThreads.Store(Conversation(a.name, channel, thread), struct{}{})
			}
			return event, false
		}
		// Unfurls change attachments without changing the user's message.
		// Mention detection reads raw text that messageText can omit.
		if e.SubType == "message_changed" && e.Message != nil && e.PreviousMessage != nil &&
			e.Message.Text == e.PreviousMessage.Text && messageText(e.Message) == messageText(e.PreviousMessage) &&
			slices.EqualFunc(e.Message.Files, e.PreviousMessage.Files, func(a, b sdk.File) bool { return a.ID == b.ID }) {
			return event, false
		}
		if thread == m.Timestamp {
			thread = ""
		}
		sender := domain.Sender{ID: m.User, DisplayName: m.Username, Kind: domain.SenderHuman}
		if m.BotID != "" {
			sender.Kind = domain.SenderBot
			if sender.ID == "" {
				sender.ID = m.BotID
			}
		}
		mentionsMe = a.selfUser != "" && strings.Contains(m.Text, "<@"+a.selfUser+">")
		event = domain.InboundEvent{
			MessageRef: Conversation(a.name, channel, m.Timestamp), Kind: kind, Deleted: deleted,
			AlsoInChannel: m.SubType == "thread_broadcast", Sender: sender, Text: messageText(m),
			MentionsMe: mentionsMe, ReceivedAt: time.Now(),
		}
		if kind == domain.InboundEventMessage {
			event.SourceKey = a.name + ":m:" + channel + ":" + m.Timestamp
		}
		posted := m.Timestamp
		if kind == domain.InboundEventMessageEdited {
			posted = e.EventTimeStamp
			if m.Edited != nil && !deleted {
				posted = m.Edited.Timestamp
			}
		}
		if at := timestamp(posted); !at.IsZero() {
			event.ReceivedAt = at
		}
		if deleted {
			event.Text = ""
			event.MentionsMe = false
		}
		for _, f := range m.Files {
			if deleted {
				break
			}
			event.Attachments = append(event.Attachments, domain.Attachment{Ref: f.ID, Name: f.Name, MIMEType: f.Mimetype})
		}
	case *slackevents.ReactionAddedEvent:
		if e.User == a.selfUser || e.Item.Type != "message" {
			return event, false
		}
		channel = e.Item.Channel
		itemAuthor = e.ItemUser
		// The sender stays a bot until users.info shows a human, so a failed lookup cannot pass a bot as a human.
		sender := domain.Sender{ID: e.User, Kind: domain.SenderBot}
		// The target timestamp keeps the reaction useful even when its message is unavailable.
		event = domain.InboundEvent{MessageRef: Conversation(a.name, channel, e.Item.Timestamp), Kind: domain.InboundEventReaction, Sender: sender, Text: fmt.Sprintf(":%s: on message %s", e.Reaction, e.Item.Timestamp), ReceivedAt: time.Now()}
		if at := timestamp(e.EventTimestamp); !at.IsZero() {
			event.ReceivedAt = at
		}
	default:
		return event, false
	}
	if channel == "" || event.Sender.ID == "" {
		return event, false
	}
	event.Direct = strings.HasPrefix(channel, "D")
	event.Conversation = Conversation(a.name, channel, thread)
	if thread != "" {
		event.Parent = Conversation(a.name, channel, "")
	}
	acceptsConversation := a.acceptConversation == nil || a.acceptConversation(Conversation(a.name, channel, ""))
	if !event.Direct {
		switch {
		case event.Kind == domain.InboundEventReaction:
			if itemAuthor != a.selfUser {
				return event, false
			}
		// In a conversation that the core ignores, the adapter skips the lookup.
		case thread != "" && !mentionsMe && acceptsConversation:
			joined, err := a.joinedThread(ctx, channel, thread)
			if err != nil {
				slog.WarnContext(ctx, "Thread participation lookup failed", "channel", channel, "thread", thread, "error", err)
			} else if !joined {
				return event, false
			}
		}
	}
	if event.Kind == domain.InboundEventReaction && acceptsConversation {
		if user, err := a.bot.GetUserInfoContext(ctx, event.Sender.ID); err == nil {
			if !user.IsBot {
				event.Sender.Kind = domain.SenderHuman
			}
			event.Sender.DisplayName = user.Profile.DisplayName
		} else {
			slog.WarnContext(ctx, "Reaction sender lookup failed", "channel", channel, "error", err)
		}
	}
	return event, true
}

func blockText(blocks sdk.Blocks) string {
	var parts []string
	appendText := func(text *sdk.TextBlockObject) {
		if text != nil && text.Text != "" {
			parts = append(parts, text.Text)
		}
	}
	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *sdk.RichTextBlock:
			return ""
		case *sdk.SectionBlock:
			appendText(b.Text)
			for _, field := range b.Fields {
				appendText(field)
			}
		case *sdk.HeaderBlock:
			appendText(b.Text)
		case *sdk.ContextBlock:
			for _, element := range b.ContextElements.Elements {
				if text, ok := element.(*sdk.TextBlockObject); ok {
					appendText(text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func messageBodyText(text string, blocks sdk.Blocks) string {
	var parts []string
	if body := cmp.Or(blockText(blocks), text); body != "" {
		parts = append(parts, body)
	}
	for _, block := range blocks.BlockSet {
		if table, ok := block.(*sdk.TableBlock); ok {
			if rendered := renderTable(table.Rows); rendered != "" {
				parts = append(parts, rendered)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

var tableCellEscapes = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>", "\r", "<br>")

func tableWidth(rows [][]sdk.TableCell) int {
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	return columns
}

func renderTable(rows [][]sdk.TableCell) string {
	columns := tableWidth(rows)
	if columns == 0 {
		return ""
	}

	var table strings.Builder
	for i, row := range rows {
		if i > 0 {
			table.WriteByte('\n')
		}
		table.WriteByte('|')
		for column := range columns {
			table.WriteByte(' ')
			if column < len(row) {
				table.WriteString(tableCellEscapes.Replace(tableCellText(row[column])))
			}
			table.WriteString(" |")
		}
		if i == 0 {
			table.WriteString("\n|" + strings.Repeat(" --- |", columns))
		}
	}
	return table.String()
}

func tableCellText(cell sdk.TableCell) string {
	switch c := cell.(type) {
	case *sdk.TableRawTextCell:
		return c.Text
	case *sdk.TableRawNumberCell:
		if c.Text != "" {
			return c.Text
		}
		return strconv.FormatFloat(c.Value, 'f', -1, 64)
	case *sdk.TableRichTextCell:
		return strings.Join(richTextLines(c.Elements), "\n")
	default:
		return ""
	}
}

func richTextLines(elements []sdk.RichTextElement) []string {
	var lines []string
	for _, element := range elements {
		switch e := element.(type) {
		case *sdk.RichTextSection:
			lines = append(lines, richTextLine(e.Elements))
		case *sdk.RichTextQuote:
			lines = append(lines, richTextLine(e.Elements))
		case *sdk.RichTextPreformatted:
			lines = append(lines, richTextLine(e.Elements))
		case *sdk.RichTextList:
			lines = append(lines, richTextLines(e.Elements)...)
		}
	}
	return lines
}

func richTextLine(elements []sdk.RichTextSectionElement) string {
	var line strings.Builder
	for _, element := range elements {
		switch e := element.(type) {
		case *sdk.RichTextSectionTextElement:
			line.WriteString(e.Text)
		case *sdk.RichTextSectionLinkElement:
			if e.Text == "" || e.Text == e.URL {
				line.WriteString("<" + e.URL + ">")
			} else {
				line.WriteString("<" + e.URL + "|" + e.Text + ">")
			}
		case *sdk.RichTextSectionUserElement:
			line.WriteString("<@" + e.UserID + ">")
		case *sdk.RichTextSectionChannelElement:
			line.WriteString("<#" + e.ChannelID + ">")
		case *sdk.RichTextSectionUserGroupElement:
			line.WriteString("<!subteam^" + e.UsergroupID + ">")
		case *sdk.RichTextSectionBroadcastElement:
			line.WriteString("<!" + e.Range + ">")
		case *sdk.RichTextSectionEmojiElement:
			line.WriteString(":" + e.Name + ":")
		case *sdk.RichTextSectionDateElement:
			if e.Fallback != nil {
				line.WriteString(*e.Fallback)
			}
		}
	}
	return line.String()
}

func messageText(m *sdk.Msg) string {
	parts := []string{messageBodyText(m.Text, m.Blocks)}
	for _, attachment := range m.Attachments {
		// People can only attach forwarded messages, so their other attachments are unfurls.
		if (m.BotID == "" && !attachment.IsShare) || (m.BotID != "" && attachment.OriginalURL != "") {
			continue
		}
		if text := blockText(attachment.Blocks); text != "" {
			parts = append(parts, text)
			continue
		}
		parts = append(parts, attachment.Title, attachment.Text)
		if attachment.Title == "" && attachment.Text == "" {
			parts = append(parts, attachment.Fallback)
		}
	}
	parts = slices.DeleteFunc(parts, func(part string) bool { return part == "" })
	if len(parts) > 1 && slices.ContainsFunc(m.Blocks.BlockSet, func(block sdk.Block) bool {
		table, ok := block.(*sdk.TableBlock)
		return ok && tableWidth(table.Rows) > 0
	}) {
		// A blank line keeps attachment text from becoming a table row.
		return parts[0] + "\n\n" + strings.Join(parts[1:], "\n")
	}
	return strings.Join(parts, "\n")
}

func (a *adapter) isSelf(user, bot string) bool {
	return (a.selfUser != "" && user == a.selfUser) || (a.selfBot != "" && bot == a.selfBot)
}

// The lookup runs on the event stream, so it must not hold later events.
const threadLookupTimeout = 2 * time.Second

func (a *adapter) joinedThread(ctx context.Context, channel, thread string) (bool, error) {
	ref := Conversation(a.name, channel, thread)
	if _, ok := a.joinedThreads.Load(ref); ok {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, threadLookupTimeout)
	defer cancel()
	params := &sdk.GetConversationRepliesParameters{ChannelID: channel, Timestamp: thread, Limit: 100}
	reader := a.bot
	for {
		messages, _, cursor, err := reader.GetConversationRepliesContext(ctx, params)
		if reader == a.bot && a.needsUserToken(err) {
			reader = a.reader
			messages, _, cursor, err = reader.GetConversationRepliesContext(ctx, params)
		}
		if err != nil {
			return false, err
		}
		for _, m := range messages {
			if a.isSelf(m.User, m.BotID) {
				a.joinedThreads.Store(ref, struct{}{})
				return true, nil
			}
		}
		if cursor == "" {
			return false, nil
		}
		params.Cursor = cursor
	}
}
