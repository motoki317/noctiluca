// The event envelope and payload fields follow traPtitech/traQ (MIT).
// Source: https://github.com/traPtitech/traQ. See THIRD_PARTY_NOTICES.md.
package traq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	sdk "github.com/traPtitech/go-traq"
	"golang.org/x/sync/errgroup"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type envelope struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

type embedded struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type wireMessage struct {
	ID      string `json:"id"`
	Channel string `json:"channelId"`
	User    struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Bot         bool   `json:"bot"`
	} `json:"user"`
	UserID    string     `json:"userId"`
	Text      string     `json:"text"`
	Embedded  []embedded `json:"embedded"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type payload struct {
	EventTime time.Time          `json:"eventTime"`
	Message   wireMessage        `json:"message"`
	MessageID string             `json:"messageId"`
	Stamps    []sdk.MessageStamp `json:"stamps"`
}

var errReconnect = errors.New("traQ WebSocket disconnected")

func New(name string, config Config, history domain.EventRepository) (domain.MessagingAdapter, error) {
	return newAdapter(name, config, history)
}

func (a *adapter) Start(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) error {
	ctx, finish, err := a.life.Begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	a.started = time.Now()
	me, response, err := a.api.MeAPI.GetMe(ctx).Execute()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return apiError("authenticate traQ bot", response, err)
	}
	if !validID(me.Id) {
		return errors.New("traQ returned an invalid bot user ID")
	}
	a.self = me.Id
	delay := time.Second
	for ctx.Err() == nil {
		endpoint := "ws" + strings.TrimPrefix(a.config.Origin, "http") + "/api/v3/bots/ws"
		conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, http.Header{"Authorization": []string{"Bearer " + a.config.AccessToken}})
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			delay = time.Second
			err = a.connected(ctx, conn, consume)
			if ctx.Err() != nil {
				return nil
			}
			if !errors.Is(err, errReconnect) {
				return err
			}
		} else if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return apiError("connect traQ WebSocket", response, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.WarnContext(ctx, "traQ WebSocket reconnecting", "entry", a.name, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	return nil
}

func (a *adapter) connected(ctx context.Context, conn *websocket.Conn, consume func(context.Context, domain.InboundEvent) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(4 << 20)
	// Freeze every cursor before live events can advance the stored history.
	targets := a.backfillTargets(ctx)
	group, ctx := errgroup.WithContext(ctx)
	stopGroup := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopGroup()
	frames := make(chan envelope, 256)
	backfillCtx, cancelBackfill := context.WithCancel(ctx)
	defer cancelBackfill()
	group.Go(func() error {
		defer close(frames)
		defer cancelBackfill()
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				return nil
			}
			if kind != websocket.TextMessage {
				continue
			}
			var message envelope
			if err := json.Unmarshal(raw, &message); err != nil {
				return errReconnect
			}
			select {
			case frames <- message:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	group.Go(func() error {
		if err := a.backfill(backfillCtx, targets, consume); err != nil {
			return err
		}
		// Live persistence must not move a cursor past an interrupted backfill.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case message, ok := <-frames:
				if !ok {
					return errReconnect
				}
				if err := a.dispatch(ctx, message, consume); err != nil {
					return err
				}
			}
		}
	})
	return group.Wait()
}

func (a *adapter) dispatch(ctx context.Context, frame envelope, consume func(context.Context, domain.InboundEvent) error) error {
	switch frame.Type {
	case "MESSAGE_CREATED", "MENTION_MESSAGE_CREATED", "MESSAGE_UPDATED", "MESSAGE_DELETED", "DIRECT_MESSAGE_CREATED", "DIRECT_MESSAGE_UPDATED", "DIRECT_MESSAGE_DELETED", "BOT_MESSAGE_STAMPS_UPDATED":
	default:
		return nil
	}
	var body payload
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		a.warnLookup(ctx, "decode traQ event", err, "event_type", frame.Type)
		return nil
	}
	if frame.Type == "BOT_MESSAGE_STAMPS_UPDATED" {
		return a.reactions(ctx, body, consume)
	}
	m := body.Message
	if !validID(m.ID) || !validID(m.Channel) {
		return nil
	}
	direct := strings.HasPrefix(frame.Type, "DIRECT_")
	event := domain.InboundEvent{Conversation: Conversation(a.name, m.Channel, ""), MessageRef: Conversation(a.name, m.Channel, m.ID), Direct: direct, Kind: domain.InboundEventMessage, Text: m.Text, ReceivedAt: m.CreatedAt, SourceKey: a.name + ":m:" + m.ID}
	switch {
	case strings.HasSuffix(frame.Type, "_DELETED"):
		if validID(m.UserID) {
			user, err := a.lookupUser(ctx, m.UserID)
			if err != nil {
				a.warnLookup(ctx, "read traQ user", err, "channel_id", m.Channel, "message_id", m.ID, "user_id", m.UserID)
				return nil
			}
			event.Sender = user
		} else {
			previous, err := a.history.GetInbound(ctx, event.MessageRef)
			if errors.Is(err, domain.ErrNotFound) {
				return nil
			}
			if err != nil {
				a.warnLookup(ctx, "look up deleted traQ message", err, "channel_id", m.Channel, "message_id", m.ID)
				return nil
			}
			event.Sender = previous.Sender
		}
		event.Kind, event.Deleted, event.Text = domain.InboundEventMessageEdited, true, ""
		event.SourceKey = a.name + ":d:" + m.ID
		event.ReceivedAt = body.EventTime
	default:
		if !validID(m.User.ID) {
			return nil
		}
		name := m.User.DisplayName
		if name == "" {
			name = m.User.Name
		}
		event.Sender = sender(m.User.ID, name, m.User.Bot)
		a.users.Store(m.User.ID, event.Sender)
		event.MentionsMe = frame.Type == "MENTION_MESSAGE_CREATED" || a.mentions(m.Text, m.Embedded)
		if strings.HasSuffix(frame.Type, "_UPDATED") {
			if m.UpdatedAt.IsZero() {
				return nil
			}
			event.Kind = domain.InboundEventMessageEdited
			event.ReceivedAt = m.UpdatedAt
			event.SourceKey = a.name + ":e:" + m.ID + ":" + m.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if event.Sender.ID == "" || event.Sender.ID == a.self {
		return nil
	}
	if direct {
		a.direct.Store(m.Channel, event.Sender.ID)
	} else {
		a.direct.Store(m.Channel, "")
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = body.EventTime
	}
	if event.ReceivedAt.IsZero() {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	return consume(ctx, event)
}

func (a *adapter) mentions(text string, entities []embedded) bool {
	for _, entity := range entities {
		if entity.Type == "user" && entity.ID == a.self {
			return true
		}
	}
	// REST returns embedded mentions in content, without the WebSocket's embedded array.
	for {
		index := strings.Index(text, "!{")
		if index < 0 {
			return false
		}
		text = text[index+1:]
		var entity embedded
		if err := json.NewDecoder(strings.NewReader(text)).Decode(&entity); err == nil && entity.Type == "user" && entity.ID == a.self {
			return true
		}
	}
}

func (a *adapter) reactionChannel(ctx context.Context, id string) (string, bool, error) {
	channel, _ := a.posted.Load(id)
	if channel == nil {
		message, response, err := a.api.MessageAPI.GetMessage(ctx, id).Execute()
		if err != nil {
			return "", false, apiError("read traQ stamp target", response, err)
		}
		if message.UserId != a.self {
			return "", false, nil
		}
		channel = message.ChannelId
		a.posted.Store(id, channel)
	}
	channelID := channel.(string)
	if direct, ok := a.direct.Load(channelID); ok {
		return channelID, direct.(string) != "", nil
	}
	channels, response, err := a.api.ChannelAPI.GetChannels(ctx).IncludeDm(true).Execute()
	if err != nil {
		return channelID, false, apiError("resolve traQ stamp channel", response, err)
	}
	for _, c := range channels.Public {
		a.direct.Store(c.Id, "")
	}
	for _, c := range channels.Dm {
		a.direct.Store(c.Id, c.UserId)
	}
	direct, _ := a.direct.Load(channelID)
	return channelID, direct != nil && direct.(string) != "", nil
}

func (a *adapter) reactions(ctx context.Context, body payload, consume func(context.Context, domain.InboundEvent) error) error {
	if !validID(body.MessageID) {
		return nil
	}
	previous := a.snapshots[body.MessageID]
	if previous == nil {
		previous = make(map[string]time.Time)
		a.snapshots[body.MessageID] = previous
	}
	for _, stamp := range body.Stamps {
		key := stamp.UserId + ":" + stamp.StampId
		if stamp.UserId == a.self || !validID(stamp.UserId) || !validID(stamp.StampId) || stamp.Count <= 0 || !stamp.UpdatedAt.After(a.started) || !stamp.UpdatedAt.After(previous[key]) {
			continue
		}
		channel, direct, err := a.reactionChannel(ctx, body.MessageID)
		if err != nil {
			a.warnLookup(ctx, "resolve traQ stamp channel", err, "channel_id", channel, "message_id", body.MessageID, "user_id", stamp.UserId, "stamp_id", stamp.StampId)
			return nil
		}
		if channel == "" {
			continue
		}
		ref := Conversation(a.name, channel, "")
		if a.acceptConversation != nil && !a.acceptConversation(ref) {
			previous[key] = stamp.UpdatedAt
			continue
		}
		name, ok := a.stamps.Load(stamp.StampId)
		if !ok {
			result, response, err := a.api.StampAPI.GetStamp(ctx, stamp.StampId).Execute()
			if err != nil {
				a.warnLookup(ctx, "read traQ stamp", apiError("read traQ stamp", response, err), "channel_id", channel, "message_id", body.MessageID, "user_id", stamp.UserId, "stamp_id", stamp.StampId)
				continue
			}
			name = result.Name
			a.stamps.Store(stamp.StampId, name)
		}
		user, err := a.lookupUser(ctx, stamp.UserId)
		if err != nil {
			a.warnLookup(ctx, "read traQ user", err, "channel_id", channel, "message_id", body.MessageID, "user_id", stamp.UserId, "stamp_id", stamp.StampId)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		event := domain.InboundEvent{SourceKey: a.name + ":r:" + body.MessageID + ":" + key, Kind: domain.InboundEventReaction, Conversation: ref, MessageRef: Conversation(a.name, channel, body.MessageID), Direct: direct, Sender: user, Text: fmt.Sprintf(":%s: on message %s", name, body.MessageID), ReceivedAt: stamp.UpdatedAt}
		if err := consume(ctx, event); err != nil {
			return err
		}
		// Retain the high-water mark across removals and out-of-order snapshots.
		previous[key] = stamp.UpdatedAt
	}
	return nil
}
