package traq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type toolInput struct {
	Channel string `json:"channel"`
	Message string `json:"message_id"`
	Quote   string `json:"quote_message_id"`
	Text    string `json:"text"`
	Query   string `json:"query"`
	Limit   int32  `json:"limit"`
	Offset  int32  `json:"offset"`
	File    string `json:"file_id"`
	Path    string `json:"path"`
	Stamp   string `json:"stamp"`
}

func (a *adapter) ToolNotes() string {
	return fmt.Sprintf("traQ has no threads. A ref ending in a colon is a channel. "+
		"A channel work target ends in the request's message ID: replying to it appends the request's URL as a quote. "+
		"With %[1]s_send or %[1]s_upload_file, set quote_message_id to that ID. Tool arguments are bare IDs. "+
		"Use %[1]s_read_message, %[1]s_read_history, and %[1]s_search for context. "+
		"Text uses Markdown and @username mentions. The 10000-character limit includes file and quote URLs. "+
		"A file ID is the UUID in a /files/<id> URL in message text.", a.name)
}

func (a *adapter) Tools() []domain.Tool {
	str := map[string]any{"type": "string", "minLength": 1}
	id := map[string]any{"type": "string", "pattern": idPattern.String()}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	offset := map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483547}
	tool := func(name, description string, properties map[string]any, required []string, handler func(context.Context, toolInput) (any, error)) domain.Tool {
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
		return domain.Tool{Name: a.name + "_" + name, Description: description, ReadOnly: name == "read_message" || name == "read_history" || name == "search", InputSchema: schema, Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var in toolInput
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&in); err != nil {
				return nil, err
			}
			for _, value := range []string{in.Channel, in.Message, in.Quote, in.File} {
				if value != "" && !validID(value) {
					return nil, errors.New("IDs must be bare traQ IDs")
				}
			}
			if in.Limit == 0 {
				in.Limit = 20
			}
			if in.Limit < 1 || in.Limit > 100 || in.Offset < 0 || in.Offset > 2147483547 {
				return nil, errors.New("limit must be 1..100 and offset 0..2147483547")
			}
			values := map[string]string{"channel": in.Channel, "message_id": in.Message, "text": in.Text, "query": in.Query, "file_id": in.File, "path": in.Path, "stamp": in.Stamp}
			for _, key := range required {
				if strings.TrimSpace(values[key]) == "" {
					return nil, fmt.Errorf("%s is required", key)
				}
			}
			result, err := handler(ctx, in)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		}}
	}
	return []domain.Tool{
		tool("send", "Post Markdown in a channel. When the work target ends in a message ID, set quote_message_id to it.", map[string]any{"channel": id, "text": str, "quote_message_id": id}, []string{"channel", "text"}, a.sendTool),
		tool("read_history", "Read channel messages, newest first. Continue with next_offset.", map[string]any{"channel": id, "limit": limit, "offset": offset}, []string{"channel"}, a.readHistory),
		tool("read_message", "Read one message by ID.", map[string]any{"message_id": id}, []string{"message_id"}, a.readMessage),
		tool("search", "Search traQ messages by words, optionally within a channel.", map[string]any{"query": str, "channel": id, "limit": limit, "offset": offset}, []string{"query"}, a.search),
		tool("download_file", "Download a file as raw bytes to a new local path.", map[string]any{"file_id": id, "path": str}, []string{"file_id", "path"}, a.download),
		tool("upload_file", "Upload a local file and post its URL. When the work target ends in a message ID, set quote_message_id to it.", map[string]any{"channel": id, "path": str, "text": str, "quote_message_id": id}, []string{"channel", "path"}, a.upload),
		tool("react", "Add a stamp to a message by stamp name.", map[string]any{"message_id": id, "stamp": str}, []string{"message_id", "stamp"}, a.react),
	}
}

func (a *adapter) readHistory(ctx context.Context, in toolInput) (any, error) {
	messages, response, err := a.api.MessageAPI.GetMessages(ctx, in.Channel).Limit(in.Limit).Offset(in.Offset).Order("desc").Execute()
	if err != nil {
		return nil, apiError("read traQ history", response, err)
	}
	return map[string]any{"messages": messages, "has_more": len(messages) == int(in.Limit), "next_offset": in.Offset + int32(len(messages))}, nil
}

func (a *adapter) readMessage(ctx context.Context, in toolInput) (any, error) {
	message, response, err := a.api.MessageAPI.GetMessage(ctx, in.Message).Execute()
	if err != nil {
		return nil, apiError("read traQ message", response, err)
	}
	return message, nil
}

func (a *adapter) search(ctx context.Context, in toolInput) (any, error) {
	request := a.api.MessageAPI.SearchMessages(ctx).Word(in.Query).Limit(in.Limit).Offset(in.Offset).Sort("-createdAt")
	if in.Channel != "" {
		request = request.In(in.Channel)
	}
	result, response, err := request.Execute()
	if err != nil {
		return nil, apiError("search traQ messages", response, err)
	}
	return map[string]any{"messages": result.Hits, "total": result.TotalHits, "next_offset": in.Offset + int32(len(result.Hits))}, nil
}

func (a *adapter) download(ctx context.Context, in toolInput) (any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.config.Origin+"/api/v3/files/"+in.File, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+a.config.AccessToken)
	// The generated SDK decodes file bodies as text instead of streaming bytes.
	response, err := a.http.Do(request)
	if err != nil {
		return nil, apiError("download traQ file", response, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, apiError("download traQ file", response, nil)
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(in.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(out, response.Body)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(in.Path)
		return nil, errors.New("download traQ file failed while writing bytes")
	}
	return map[string]string{"path": in.Path, "id": in.File}, nil
}

func (a *adapter) upload(ctx context.Context, in toolInput) (any, error) {
	ref := Conversation(a.name, in.Channel, in.Quote)
	// File IDs are UUIDs, so the final URL length is known before upload.
	if _, _, err := a.messageText(ref, domain.OutboundMessage{Text: in.Text, Attachments: []domain.Attachment{{Ref: "00000000-0000-0000-0000-000000000000"}}}); err != nil {
		return nil, err
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
	result, response, err := a.api.FileAPI.PostFile(ctx).File(file).ChannelId(in.Channel).Execute()
	if err != nil {
		return nil, apiError("upload traQ file", response, err)
	}
	message := domain.OutboundMessage{Text: in.Text, Attachments: []domain.Attachment{{Ref: result.Id, Name: result.Name, MIMEType: result.Mime}}}
	id, err := a.Send(ctx, ref, message)
	if err != nil {
		return nil, fmt.Errorf("file %s uploaded but posting its URL failed: %w", result.Id, err)
	}
	if err := a.record(ctx, id, ref, message); err != nil {
		return nil, err
	}
	return map[string]string{"id": result.Id, "message_id": id, "url": a.config.Origin + "/files/" + result.Id}, nil
}

func (a *adapter) react(ctx context.Context, in toolInput) (any, error) {
	stamps, response, err := a.api.StampAPI.GetStamps(ctx).Execute()
	if err != nil {
		return nil, apiError("resolve traQ stamp", response, err)
	}
	name := strings.Trim(in.Stamp, ":")
	for _, stamp := range stamps {
		if stamp.Name != name {
			continue
		}
		response, err := a.api.MessageAPI.AddMessageStamp(ctx, in.Message, stamp.Id).PostMessageStampRequest(*sdk.NewPostMessageStampRequest(1)).Execute()
		if err != nil {
			return nil, apiError("add traQ stamp", response, err)
		}
		return map[string]bool{"ok": true}, nil
	}
	return nil, fmt.Errorf("unknown traQ stamp %q", name)
}

func (a *adapter) sendTool(ctx context.Context, in toolInput) (any, error) {
	ref := Conversation(a.name, in.Channel, in.Quote)
	message := domain.OutboundMessage{Text: in.Text}
	id, err := a.Send(ctx, ref, message)
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
		return errors.New("posted to traQ but history recording failed")
	}
	return nil
}
