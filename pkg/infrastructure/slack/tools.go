package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/slack-go/slack"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type toolInput struct {
	Channel   string `json:"channel"`
	Thread    string `json:"thread_ts"`
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"`
	Status    string `json:"status"`
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

func (a *adapter) ToolNotes(ctx context.Context) string {
	notes := fmt.Sprintf(`Use bare Slack channel IDs. The input's channel, thread_ts, and timestamp identify the channel, thread, and message.
For a top-level input, omit thread_ts for chat and quick answers. For work, set thread_ts to timestamp to start a thread.
For an existing thread, copy thread_ts. In a DM, reply in the conversation where the input arrived.
Write %[1]s_send text and %[1]s_upload_file captions in Markdown, not in the Slack mrkdwn that inputs and read tools show:
- Bold takes two asterisks, because one makes italics.
- Start each list item with "- " or "1. ", because "•" stays plain text.
- Write each URL as <url>, <url|label>, or [label](url). A bare URL that touches other text links wrongly or not at all.
- Mentions such as <@U…> and <#C…> work as in mrkdwn.
- A pipe table shows as a table.
If an answer needs any tool besides %[1]s_send, first show that you started, in the place where you will answer.
For a thread, including one that you start, call %[1]s_set_status. Call it again when work continues after a post. At top level, call %[1]s_react with channel and timestamp.`, a.name)
	if a.owner == "" {
		return notes
	}
	a.ownerDMMu.Lock()
	defer a.ownerDMMu.Unlock()
	if a.ownerDM == "" {
		ref, err := a.DirectMessage(ctx, a.owner)
		if err != nil {
			slog.WarnContext(ctx, "Resolve owner DM for tool notes failed", "messaging", a.name, "error", err)
			return notes
		}
		channel, _, err := a.destination(ref)
		if err != nil {
			slog.WarnContext(ctx, "Invalid owner DM for tool notes", "messaging", a.name, "error", err)
			return notes
		}
		a.ownerDM = channel
	}
	return notes + fmt.Sprintf("\nOwner DM: use %s_send with channel=%s and no thread_ts.", a.name, a.ownerDM)
}

func (a *adapter) Tools() []domain.Tool {
	str := map[string]any{"type": "string", "minLength": 1}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	tool := func(name, description string, properties map[string]any, required []string, handler func(context.Context, toolInput) (any, error)) domain.Tool {
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
		return domain.Tool{MainSessionOnly: name == "send" || name == "upload_file" || name == "react" || name == "unreact" || name == "set_status", Name: a.name + "_" + name, ReadOnly: name == "read_thread" || name == "read_history" || name == "search", Description: description, InputSchema: schema, Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
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
	reactionProperties := map[string]any{"channel": str, "timestamp": str, "emoji": str}
	reactionRequired := []string{"channel", "timestamp", "emoji"}
	// setStatus also sends the text as a loading message, and Slack rejects one over 50 characters.
	status := map[string]any{"type": "string", "minLength": 1, "maxLength": 50}
	return []domain.Tool{
		tool("read_thread", "Read a thread. Returns messages and the next cursor.", map[string]any{"channel": str, "thread_ts": str, "limit": limit, "cursor": str}, []string{"channel", "thread_ts"}, a.readThread),
		tool("read_history", "Read recent channel messages. Returns messages and the next cursor.", map[string]any{"channel": str, "limit": limit, "cursor": str}, []string{"channel"}, a.readHistory),
		tool("search", "Search Slack messages.", map[string]any{"query": str, "limit": limit, "page": map[string]any{"type": "integer", "minimum": 1}}, []string{"query"}, a.search),
		tool("download_file", "Download a file to a new local path as raw bytes.", map[string]any{"file_id": str, "path": str}, []string{"file_id", "path"}, a.download),
		tool("upload_file", "Upload a local file. Omit thread_ts for a top-level post. Set broadcast with thread_ts to show a PNG, JPEG, or GIF image in the channel too. Read tools show no files on a broadcast image.", map[string]any{"channel": str, "thread_ts": str, "path": str, "title": str, "text": str, "broadcast": map[string]any{"type": "boolean"}}, []string{"channel", "path"}, a.upload),
		tool("react", "Add an emoji reaction to a message.", reactionProperties, reactionRequired, a.react),
		tool("unreact", "Remove your own emoji reaction from a message.", reactionProperties, reactionRequired, a.unreact),
		tool("set_status", "Show status text with a loading animation in a thread. Slack shows the text exactly as given, without the app name. Example status: \"Looking into it…\". Slack clears it when you post in that thread, or after two minutes without a post.", map[string]any{"channel": str, "thread_ts": str, "status": status}, []string{"channel", "thread_ts", "status"}, a.setStatus),
		tool("send", domain.PostingGuidance+" Omit thread_ts for a top-level post. Set broadcast on a thread reply to also show it in the channel.", map[string]any{"channel": str, "thread_ts": str, "text": str, "broadcast": map[string]any{"type": "boolean"}}, []string{"channel", "text"}, a.sendTool),
	}
}

type messageView struct {
	User        string              `json:"user,omitempty"`
	Text        string              `json:"text"`
	Timestamp   string              `json:"ts"`
	Thread      string              `json:"thread_ts,omitempty"`
	Files       []domain.Attachment `json:"files,omitempty"`
	Reactions   []reactionView      `json:"reactions,omitempty"`
	Attachments []attachmentView    `json:"attachments,omitempty"`
}

type attachmentView struct {
	Title    string `json:"title,omitempty"`
	Text     string `json:"text,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

func attachmentsView(attachments []sdk.Attachment) []attachmentView {
	var result []attachmentView
	for _, a := range attachments {
		if text := blockText(a.Blocks); text != "" {
			result = append(result, attachmentView{Text: text})
		} else if a.Title != "" || a.Text != "" || a.Fallback != "" {
			result = append(result, attachmentView{Title: a.Title, Text: a.Text, Fallback: a.Fallback})
		}
	}
	return result
}

type reactionView struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func messagesView(messages []sdk.Message) []messageView {
	result := make([]messageView, 0, len(messages))
	for _, m := range messages {
		v := messageView{User: m.User, Text: messageBodyText(m.Text, m.Blocks), Timestamp: m.Timestamp, Thread: m.ThreadTimestamp}
		v.Attachments = attachmentsView(m.Attachments)
		for _, f := range m.Files {
			v.Files = append(v.Files, domain.Attachment{Ref: f.ID, Name: f.Name, MIMEType: f.Mimetype})
		}
		for _, r := range m.Reactions {
			v.Reactions = append(v.Reactions, reactionView{Name: r.Name, Count: r.Count})
		}
		result = append(result, v)
	}
	return result
}

func slackError(operation string, err error) error {
	var response sdk.SlackErrorResponse
	if errors.As(err, &response) {
		return fmt.Errorf("%s: %s", operation, response.Err)
	}
	return errors.New(operation)
}

func (a *adapter) needsUserToken(err error) bool {
	var response sdk.SlackErrorResponse
	return a.reader != a.bot && errors.As(err, &response) && (response.Err == "not_in_channel" || response.Err == "channel_not_found")
}

func (a *adapter) readThread(ctx context.Context, in toolInput) (any, error) {
	params := &sdk.GetConversationRepliesParameters{ChannelID: in.Channel, Timestamp: in.Thread, Limit: in.Limit, Cursor: in.Cursor}
	messages, more, cursor, err := a.bot.GetConversationRepliesContext(ctx, params)
	if a.needsUserToken(err) {
		messages, more, cursor, err = a.reader.GetConversationRepliesContext(ctx, params)
	}
	if err != nil {
		return nil, slackError("read Slack thread failed", err)
	}
	return map[string]any{"messages": messagesView(messages), "has_more": more, "next_cursor": cursor}, nil
}

func (a *adapter) readHistory(ctx context.Context, in toolInput) (any, error) {
	params := &sdk.GetConversationHistoryParameters{ChannelID: in.Channel, Limit: in.Limit, Cursor: in.Cursor}
	result, err := a.bot.GetConversationHistoryContext(ctx, params)
	if a.needsUserToken(err) {
		result, err = a.reader.GetConversationHistoryContext(ctx, params)
	}
	if err != nil {
		return nil, slackError("read Slack history failed", err)
	}
	return map[string]any{"messages": messagesView(result.Messages), "has_more": result.HasMore, "next_cursor": result.ResponseMetaData.NextCursor}, nil
}

func (a *adapter) search(ctx context.Context, in toolInput) (any, error) {
	result, err := a.reader.SearchMessagesContext(ctx, in.Query, sdk.SearchParameters{Count: in.Limit, Page: in.Page, Sort: "timestamp", SortDirection: "desc"})
	if err != nil {
		return nil, slackError("search Slack messages failed", err)
	}
	matches := make([]map[string]any, 0, len(result.Matches))
	for _, m := range result.Matches {
		match := map[string]any{"channel": m.Channel.ID, "user": m.User, "text": messageBodyText(m.Text, m.Blocks), "ts": m.Timestamp, "permalink": m.Permalink}
		if attachments := attachmentsView(m.Attachments); len(attachments) > 0 {
			match["attachments"] = attachments
		}
		matches = append(matches, match)
	}
	return map[string]any{"matches": matches, "total": result.Total, "page": in.Page}, nil
}

func (a *adapter) download(ctx context.Context, in toolInput) (any, error) {
	file, _, _, err := a.reader.GetFileInfoContext(ctx, in.FileID, 0, 0)
	if err != nil {
		return nil, slackError("read Slack file failed", err)
	}
	if file.URLPrivateDownload == "" {
		return nil, errors.New("file has no Slack private download URL")
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0o700); err != nil {
		return nil, err
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
		return nil, slackError("download Slack file failed", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(in.Path)
		return nil, err
	}
	return map[string]string{"path": in.Path, "id": file.ID}, nil
}

func (a *adapter) upload(ctx context.Context, in toolInput) (any, error) {
	if in.Broadcast && in.Thread == "" {
		return nil, errors.New("broadcast requires thread_ts")
	}
	if utf8.RuneCountInString(in.Text) > 12000 {
		return nil, errors.New("upload caption must be at most 12000 characters")
	}
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
	filename := filepath.Base(in.Path)
	params := sdk.UploadFileParameters{Reader: file, FileSize: int(stat.Size()), Filename: filename, Title: in.Title, Channel: in.Channel, ThreadTimestamp: in.Thread}
	if in.Broadcast {
		var header [512]byte
		n, err := file.ReadAt(header[:], 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		switch http.DetectContentType(header[:n]) {
		case "image/png", "image/jpeg", "image/gif":
		default:
			return nil, errors.New("broadcast requires a PNG, JPEG, or GIF image")
		}
		params.Channel, params.ThreadTimestamp = "", ""
	} else if in.Text != "" {
		params.Blocks.BlockSet = []sdk.Block{sdk.NewMarkdownBlock("", in.Text)}
	}
	result, err := a.bot.UploadFileContext(ctx, params)
	if err != nil {
		return nil, slackError("upload Slack file failed", err)
	}
	id := result.ID
	if in.Broadcast {
		id, err = a.broadcastImage(ctx, in, result.ID, filename)
		if err != nil {
			return nil, err
		}
	}
	message := domain.OutboundMessage{Text: in.Text, Attachments: []domain.Attachment{{Ref: result.ID, Name: filename}}}
	if err := a.record(ctx, id, Conversation(a.name, in.Channel, in.Thread), message); err != nil {
		return nil, err
	}
	// The user token may lack access to files the bot uploads.
	info, _, _, err := a.bot.GetFileInfoContext(ctx, result.ID, 0, 0)
	var response sdk.SlackErrorResponse
	if a.reader != a.bot && errors.As(err, &response) && response.Err == "missing_scope" {
		info, _, _, err = a.reader.GetFileInfoContext(ctx, result.ID, 0, 0)
	}
	if err != nil {
		return map[string]string{"id": result.ID, "warning": "Uploaded, but could not read the permalink."}, nil
	}
	return map[string]string{"id": result.ID, "permalink": info.Permalink}, nil
}

func (a *adapter) broadcastImage(ctx context.Context, in toolInput, fileID, filename string) (string, error) {
	var blocks []sdk.Block
	if in.Text != "" {
		blocks = append(blocks, sdk.NewMarkdownBlock("", in.Text))
	}
	blocks = append(blocks, sdk.NewImageBlockSlackFile(&sdk.SlackFileObject{ID: fileID}, filename, "", nil))
	for attempt := 0; ; attempt++ {
		_, timestamp, err := a.bot.PostMessageContext(ctx, in.Channel, sdk.MsgOptionTS(in.Thread), sdk.MsgOptionBroadcast(), sdk.MsgOptionBlocks(blocks...))
		if err == nil {
			return timestamp, nil
		}
		var response sdk.SlackErrorResponse
		if attempt == 5 || !errors.As(err, &response) || response.Err != "invalid_blocks" {
			return "", slackError("broadcast Slack image failed", err)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *adapter) react(ctx context.Context, in toolInput) (any, error) {
	if err := a.bot.AddReactionContext(ctx, strings.Trim(in.Emoji, ":"), sdk.ItemRef{Channel: in.Channel, Timestamp: in.Timestamp}); err != nil {
		return nil, slackError("add Slack reaction failed", err)
	}
	return map[string]bool{"ok": true}, nil
}

func (a *adapter) unreact(ctx context.Context, in toolInput) (any, error) {
	if err := a.bot.RemoveReactionContext(ctx, strings.Trim(in.Emoji, ":"), sdk.ItemRef{Channel: in.Channel, Timestamp: in.Timestamp}); err != nil {
		return nil, slackError("remove Slack reaction failed", err)
	}
	return map[string]bool{"ok": true}, nil
}

func (a *adapter) setStatus(ctx context.Context, in toolInput) (any, error) {
	// slack-go joins with commas, so send a JSON array to keep commas inside a single loading message.
	loadingMessages, _ := json.Marshal([]string{in.Status})
	if err := a.bot.SetAssistantThreadsStatusContext(ctx, sdk.AssistantThreadsSetStatusParameters{
		ChannelID:       in.Channel,
		ThreadTS:        in.Thread,
		Status:          in.Status,
		LoadingMessages: []string{string(loadingMessages)},
	}); err != nil {
		return nil, slackError("set Slack thread status failed", err)
	}
	return map[string]bool{"ok": true}, nil
}

func (a *adapter) sendTool(ctx context.Context, in toolInput) (any, error) {
	ref := Conversation(a.name, in.Channel, in.Thread)
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
