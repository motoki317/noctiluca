package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type toolInput struct {
	Channel string `json:"channel"`
	Thread  string `json:"thread"`
	Message string `json:"message"`
	Text    string `json:"text"`
	Before  string `json:"before"`
	Limit   int    `json:"limit"`
	Query   string `json:"query"`
	Offset  int    `json:"offset"`
	FileRef string `json:"file_ref"`
	Path    string `json:"path"`
	Emoji   string `json:"emoji"`
}

func (a *adapter) ToolNotes() string {
	return fmt.Sprintf("Channel, thread, and message arguments are bare numeric IDs. A ref ending in a colon is a channel. In a ref with a final ID, that ID is a thread or a message; posting to a message ref starts a thread from it. Use %[1]s_read_history and %[1]s_read_thread for context. Only user mentions notify.", a.name)
}

func (a *adapter) Tools() []domain.Tool {
	str := map[string]any{"type": "string", "minLength": 1}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	makeTool := func(name, description string, props map[string]any, required []string, handler func(context.Context, toolInput) (any, error)) domain.Tool {
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false})
		return domain.Tool{Name: a.name + "_" + name, Description: description, ReadOnly: name == "read_history" || name == "read_thread" || name == "search", InputSchema: schema, Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, err
			}
			for key := range fields {
				if _, ok := props[key]; !ok {
					return nil, fmt.Errorf("unknown argument %q", key)
				}
			}
			for _, key := range required {
				var value string
				if err := json.Unmarshal(fields[key], &value); err != nil || value == "" {
					return nil, fmt.Errorf("%s is required", key)
				}
			}
			var in toolInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			for _, id := range []string{in.Channel, in.Thread, in.Message, in.Before} {
				if id != "" {
					if _, err := parseID(id); err != nil {
						return nil, err
					}
				}
			}
			if in.Limit == 0 {
				in.Limit = 20
			}
			if in.Limit < 1 || in.Limit > 100 {
				return nil, errors.New("limit must be between 1 and 100")
			}
			result, err := handler(ctx, in)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		}}
	}
	return []domain.Tool{
		makeTool("send", "Send a message to channel. With thread, post in that thread, or start a thread from the message with that ID.", map[string]any{"channel": str, "thread": str, "text": str}, []string{"channel", "text"}, a.sendTool),
		makeTool("read_history", "Read recent channel messages, newest first. Paginate with before.", map[string]any{"channel": str, "before": str, "limit": limit}, []string{"channel"}, a.readHistory),
		makeTool("read_thread", "Read a thread's starter and replies, newest first. Paginate replies with before.", map[string]any{"channel": str, "thread": str, "before": str, "limit": limit}, []string{"channel", "thread"}, a.readThread),
		makeTool("search", "Search messages in the guild that contains channel. DM channels cannot be searched.", map[string]any{"channel": str, "query": str, "offset": map[string]any{"type": "integer", "minimum": 0, "maximum": 9975}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 25}}, []string{"channel", "query"}, a.search),
		makeTool("react", "Add a Unicode emoji or a custom emoji in name:id form.", map[string]any{"channel": str, "message": str, "emoji": str}, []string{"channel", "message", "emoji"}, a.react),
		makeTool("download_file", "Download an attachment to a new local file using its file_ref.", map[string]any{"file_ref": str, "path": str}, []string{"file_ref", "path"}, a.download),
		makeTool("upload_file", "Upload a local file to a channel or thread.", map[string]any{"channel": str, "thread": str, "path": str, "text": str}, []string{"channel", "path"}, a.upload),
	}
}

func (a *adapter) sendTool(ctx context.Context, in toolInput) (any, error) {
	ref := Conversation(a.name, in.Channel, in.Thread)
	message := domain.OutboundMessage{Text: in.Text}
	id, err := a.Send(ctx, ref, message)
	if err != nil {
		return nil, err
	}
	if err = a.record(ctx, id, ref, message); err != nil {
		return nil, err
	}
	return map[string]string{"id": id}, nil
}

type messageView struct {
	ID          string              `json:"id"`
	Channel     string              `json:"channel"`
	User        string              `json:"user"`
	Text        string              `json:"text"`
	Attachments []domain.Attachment `json:"attachments,omitempty"`
}

func (a *adapter) views(messages []sdk.Message) []messageView {
	views := make([]messageView, 0, len(messages))
	for _, m := range messages {
		views = append(views, messageView{ID: m.ID.String(), Channel: m.ChannelID.String(), User: m.Author.ID.String(), Text: m.Content, Attachments: a.attachments(m)})
	}
	return views
}

func (a *adapter) historyPage(ctx context.Context, channel snowflake.ID, in toolInput) (any, error) {
	var before snowflake.ID
	if in.Before != "" {
		before, _ = parseID(in.Before)
	}
	ms, err := a.api.GetMessages(channel, 0, before, 0, in.Limit, rest.WithCtx(ctx))
	if err != nil {
		return nil, discordError("read Discord history failed", err)
	}
	cursor := ""
	if len(ms) == in.Limit {
		oldest := ms[0].ID
		for _, m := range ms {
			if m.ID < oldest {
				oldest = m.ID
			}
		}
		cursor = oldest.String()
	}
	return map[string]any{"messages": a.views(ms), "next_before": cursor}, nil
}
func (a *adapter) readHistory(ctx context.Context, in toolInput) (any, error) {
	id, _ := parseID(in.Channel)
	return a.historyPage(ctx, id, in)
}

func (a *adapter) readThread(ctx context.Context, in toolInput) (any, error) {
	parent, _ := parseID(in.Channel)
	thread, _ := parseID(in.Thread)
	root, err := a.api.GetMessage(parent, thread, rest.WithCtx(ctx))
	if err != nil && !rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownMessage) {
		return nil, discordError("read Discord thread starter failed", err)
	}
	var starter []messageView
	if err == nil {
		starter = a.views([]sdk.Message{*root})
	}
	c, err := a.api.GetChannel(thread, rest.WithCtx(ctx))
	if rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownChannel) && root != nil {
		return map[string]any{"starter": starter, "messages": []messageView{}}, nil
	}
	if err != nil {
		return nil, discordError("read Discord thread failed", err)
	}
	if _, err = threadTarget(c, parent); err != nil {
		return nil, err
	}
	page, err := a.historyPage(ctx, thread, in)
	if err != nil {
		return nil, err
	}
	page.(map[string]any)["starter"] = starter
	return page, nil
}

func (a *adapter) search(ctx context.Context, in toolInput) (any, error) {
	if in.Limit > 25 || in.Offset < 0 || in.Offset > 9975 || utf8.RuneCountInString(in.Query) > 1024 {
		return nil, errors.New("search requires limit 1–25, offset 0–9975, and query up to 1024 characters")
	}
	channel, _ := parseID(in.Channel)
	location, err := a.location(ctx, channel)
	if err != nil {
		return nil, discordError("resolve Discord search channel failed", err)
	}
	if location.direct || location.guild == 0 {
		return nil, errors.New("search requires a guild channel; DM channels cannot be searched")
	}
	result, err := a.api.SearchGuildMessages(ctx, location.guild, sdk.GuildMessagesSearch{Content: in.Query, Limit: in.Limit, Offset: in.Offset}, rest.WithCtx(ctx))
	if err != nil {
		return nil, discordError("search Discord guild failed", err)
	}
	return map[string]any{"messages": a.views(result.Messages), "total": result.TotalResults}, nil
}

func (a *adapter) react(ctx context.Context, in toolInput) (any, error) {
	if !utf8.ValidString(in.Emoji) || strings.TrimSpace(in.Emoji) != in.Emoji {
		return nil, errors.New("invalid Discord emoji")
	}
	if name, id, custom := strings.Cut(in.Emoji, ":"); custom {
		if name == "" {
			return nil, errors.New("custom emoji requires name:id")
		}
		if _, err := parseID(id); err != nil {
			return nil, err
		}
	}
	channel, _ := parseID(in.Channel)
	message, _ := parseID(in.Message)
	if err := a.api.AddReaction(channel, message, url.PathEscape(in.Emoji), rest.WithCtx(ctx)); err != nil {
		return nil, discordError("add Discord reaction failed", err)
	}
	return map[string]bool{"ok": true}, nil
}
