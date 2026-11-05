package slack

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/slack-go/slack"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type toolInput struct {
	Channel   string `json:"channel"`
	Thread    string `json:"thread_ts"`
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"`
	Broadcast bool   `json:"broadcast"`
	Query     string `json:"query"`
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
	Page      int    `json:"page"`
	FileID    string `json:"file_id"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	Emoji     string `json:"emoji"`
}

func (a *adapter) ToolNotes() string {
	return "Use slack_read_thread, slack_read_history, and slack_search to retrieve conversation context. " +
		"Reads use the configured user token when available. Posts always use the bot token. " +
		"Pass bare channel IDs to Slack tools. To broadcast a thread upload, call slack_send with its permalink and broadcast=true."
}

func (a *adapter) Tools() []domain.Tool {
	str := map[string]any{"type": "string", "minLength": 1}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	tool := func(name, description string, properties map[string]any, required []string, handler func(context.Context, toolInput) (any, error)) domain.Tool {
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
		return domain.Tool{Name: name, ReadOnly: name == "slack_read_thread" || name == "slack_read_history" || name == "slack_search", Description: description, InputSchema: schema, Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input toolInput
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, err
			}
			if input.Channel != "" && strings.ContainsAny(input.Channel, ": \t\n\r") {
				return nil, errors.New("channel must be a bare Slack channel ID")
			}
			if input.Limit == 0 {
				input.Limit = 20
			}
			if input.Page == 0 {
				input.Page = 1
			}
			result, err := handler(ctx, input)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		}}
	}
	return []domain.Tool{
		tool("slack_read_thread", "Read a thread. Returns messages and the next cursor.", map[string]any{"channel": str, "thread_ts": str, "limit": limit, "cursor": str}, []string{"channel", "thread_ts"}, a.readThread),
		tool("slack_read_history", "Read recent channel messages. Returns messages and the next cursor.", map[string]any{"channel": str, "limit": limit, "cursor": str}, []string{"channel"}, a.readHistory),
		tool("slack_search", "Search Slack messages.", map[string]any{"query": str, "limit": limit, "page": map[string]any{"type": "integer", "minimum": 1}}, []string{"query"}, a.search),
		tool("slack_download_file", "Download a file to a new local path as raw bytes.", map[string]any{"file_id": str, "path": str}, []string{"file_id", "path"}, a.download),
		tool("slack_upload_file", "Upload a local file. To broadcast a thread upload, use slack_send with its permalink.", map[string]any{"channel": str, "thread_ts": str, "path": str, "title": str, "text": str}, []string{"channel", "path"}, a.upload),
		tool("slack_react", "Add an emoji reaction using a bare channel ID and message timestamp.", map[string]any{"channel": str, "timestamp": str, "emoji": str}, []string{"channel", "timestamp", "emoji"}, a.react),
		tool("slack_send", "Post Markdown text. Set broadcast on a thread reply to also show it in the channel.", map[string]any{"channel": str, "thread_ts": str, "text": str, "broadcast": map[string]any{"type": "boolean"}}, []string{"channel", "text"}, a.sendTool),
	}
}

type messageView struct {
	User      string              `json:"user,omitempty"`
	Text      string              `json:"text"`
	Timestamp string              `json:"ts"`
	Thread    string              `json:"thread_ts,omitempty"`
	Files     []domain.Attachment `json:"files,omitempty"`
}

func messagesView(messages []sdk.Message) []messageView {
	result := make([]messageView, 0, len(messages))
	for _, m := range messages {
		v := messageView{User: m.User, Text: m.Text, Timestamp: m.Timestamp, Thread: m.ThreadTimestamp}
		for _, f := range m.Files {
			v.Files = append(v.Files, domain.Attachment{Ref: f.ID, Name: f.Name, MIMEType: f.Mimetype})
		}
		result = append(result, v)
	}
	return result
}

func (a *adapter) readThread(ctx context.Context, in toolInput) (any, error) {
	messages, more, cursor, err := a.reader.GetConversationRepliesContext(ctx, &sdk.GetConversationRepliesParameters{ChannelID: in.Channel, Timestamp: in.Thread, Limit: in.Limit, Cursor: in.Cursor})
	if err != nil {
		return nil, errors.New("read Slack thread failed")
	}
	return map[string]any{"messages": messagesView(messages), "has_more": more, "next_cursor": cursor}, nil
}

func (a *adapter) readHistory(ctx context.Context, in toolInput) (any, error) {
	result, err := a.reader.GetConversationHistoryContext(ctx, &sdk.GetConversationHistoryParameters{ChannelID: in.Channel, Limit: in.Limit, Cursor: in.Cursor})
	if err != nil {
		return nil, errors.New("read Slack history failed")
	}
	return map[string]any{"messages": messagesView(result.Messages), "has_more": result.HasMore, "next_cursor": result.ResponseMetaData.NextCursor}, nil
}

func (a *adapter) search(ctx context.Context, in toolInput) (any, error) {
	result, err := a.reader.SearchMessagesContext(ctx, in.Query, sdk.SearchParameters{Count: in.Limit, Page: in.Page, Sort: "timestamp", SortDirection: "desc"})
	if err != nil {
		return nil, errors.New("search Slack messages failed")
	}
	matches := make([]map[string]string, 0, len(result.Matches))
	for _, m := range result.Matches {
		matches = append(matches, map[string]string{"channel": m.Channel.ID, "user": m.User, "text": m.Text, "ts": m.Timestamp, "permalink": m.Permalink})
	}
	return map[string]any{"matches": matches, "total": result.Total, "page": in.Page}, nil
}

func (a *adapter) download(ctx context.Context, in toolInput) (any, error) {
	file, _, _, err := a.reader.GetFileInfoContext(ctx, in.FileID, 0, 0)
	if err != nil {
		return nil, errors.New("read Slack file failed")
	}
	if file.URLPrivateDownload == "" {
		return nil, errors.New("file has no Slack private download URL")
	}
	out, err := os.OpenFile(in.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	// The SDK sends bearer authentication and streams bytes without text conversion.
	if err := a.reader.GetFileContext(ctx, file.URLPrivateDownload, out); err != nil {
		_ = out.Close()
		_ = os.Remove(in.Path)
		return nil, errors.New("download Slack file failed")
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(in.Path)
		return nil, err
	}
	return map[string]string{"path": in.Path, "id": file.ID}, nil
}

func (a *adapter) upload(ctx context.Context, in toolInput) (any, error) {
	file, err := os.Open(in.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil, errors.New("upload requires a nonempty regular file")
	}
	result, err := a.bot.UploadFileContext(ctx, sdk.UploadFileParameters{Reader: file, FileSize: int(stat.Size()), Filename: filepath.Base(in.Path), Title: in.Title, Channel: in.Channel, ThreadTimestamp: in.Thread, InitialComment: mrkdwn(in.Text)})
	if err != nil {
		return nil, errors.New("upload Slack file failed")
	}
	message := domain.OutboundMessage{Text: in.Text, Attachments: []domain.Attachment{{Ref: result.ID, Name: filepath.Base(in.Path)}}}
	if err := a.record(ctx, result.ID, Conversation(in.Channel, in.Thread), message); err != nil {
		return nil, err
	}
	info, _, _, err := a.reader.GetFileInfoContext(ctx, result.ID, 0, 0)
	if err != nil {
		return map[string]string{"id": result.ID, "warning": "Uploaded, but could not read the permalink."}, nil
	}
	return map[string]string{"id": result.ID, "permalink": info.Permalink}, nil
}

func (a *adapter) react(ctx context.Context, in toolInput) (any, error) {
	if err := a.bot.AddReactionContext(ctx, strings.Trim(in.Emoji, ":"), sdk.ItemRef{Channel: in.Channel, Timestamp: in.Timestamp}); err != nil {
		return nil, errors.New("add Slack reaction failed")
	}
	return map[string]bool{"ok": true}, nil
}

func (a *adapter) sendTool(ctx context.Context, in toolInput) (any, error) {
	ref := Conversation(in.Channel, in.Thread)
	message := domain.OutboundMessage{Text: in.Text}
	id, err := a.post(ctx, ref, message, in.Broadcast)
	if err != nil {
		return nil, err
	}
	if err := a.record(ctx, id, ref, message); err != nil {
		return nil, err
	}
	return map[string]string{"id": id}, nil
}

func (a *adapter) record(ctx context.Context, id string, ref domain.ConversationRef, message domain.OutboundMessage) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.history.SaveOutbound(ctx, domain.SentMessage{ID: id, Conversation: ref, Message: message, SentAt: time.Now()}); err != nil {
		return errors.New("posted to Slack but history recording failed")
	}
	return nil
}
