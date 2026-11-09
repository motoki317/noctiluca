package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (a *adapter) dispatch(ctx context.Context, data gateway.EventData, consume func(context.Context, domain.InboundEvent) error) error {
	var event *domain.InboundEvent
	var err error
	var channel, messageID snowflake.ID
	switch e := data.(type) {
	case gateway.EventReady:
		a.self.Store(uint64(e.User.ID))
		return a.backfill(ctx, consume)
	case gateway.EventResumed:
		return a.backfill(ctx, consume)
	case gateway.EventMessageCreate:
		channel, messageID = e.ChannelID, e.ID
		event, err = a.message(ctx, e.Message, false)
	case gateway.EventMessageUpdate:
		channel, messageID = e.ChannelID, e.ID
		if e.EditedTimestamp == nil {
			return nil
		}
		// Updates can omit author, mentions and attachments; fetch the complete message.
		var message *sdk.Message
		message, err = a.api.GetMessage(e.ChannelID, e.ID, rest.WithCtx(ctx))
		if rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownMessage) {
			return nil
		}
		if err == nil {
			event, err = a.message(ctx, *message, true)
		} else {
			err = &lookupError{"GetMessage", err}
		}
	case gateway.EventMessageDelete:
		channel, messageID = e.ChannelID, e.ID
		event, err = a.deleted(ctx, e.ChannelID, e.ID)
	case gateway.EventMessageDeleteBulk:
		for _, id := range e.IDs {
			if err := a.dispatch(ctx, gateway.EventMessageDelete{ID: id, ChannelID: e.ChannelID}, consume); err != nil {
				return err
			}
		}
		return nil
	case gateway.EventMessageReactionAdd:
		channel, messageID = e.ChannelID, e.MessageID
		event, err = a.reaction(ctx, e)
	}
	if err != nil {
		a.warnLookup(ctx, channel, messageID, err)
		return nil
	}
	if event != nil && ctx.Err() == nil {
		return consume(ctx, *event)
	}
	return nil
}

type channelLocation struct {
	conversation domain.ConversationRef
	parent       domain.ConversationRef
	direct       bool
	thread       *sdk.GuildThread
	guild        snowflake.ID
}

func (a *adapter) location(ctx context.Context, id snowflake.ID) (channelLocation, error) {
	a.locationsMu.Lock()
	cached, ok := a.locations[id]
	a.locationsMu.Unlock()
	if ok {
		return cached, nil
	}
	channel, err := a.api.GetChannel(id, rest.WithCtx(ctx))
	if err != nil {
		return channelLocation{}, &lookupError{"GetChannel", err}
	}
	location := channelLocation{conversation: Conversation(a.name, id.String(), ""),
		direct: channel.Type() == sdk.ChannelTypeDM || channel.Type() == sdk.ChannelTypeGroupDM}
	if guild, ok := channel.(sdk.GuildChannel); ok {
		location.guild = guild.GuildID()
	}
	if thread, ok := channel.(sdk.GuildThread); ok {
		if thread.ParentID() == nil {
			return channelLocation{}, errors.New("thread has no parent in Discord")
		}
		location.parent = Conversation(a.name, thread.ParentID().String(), "")
		location.conversation = Conversation(a.name, thread.ParentID().String(), id.String())
		location.thread = &thread
	}
	a.locationsMu.Lock()
	a.locations[id] = location
	a.locationsMu.Unlock()
	return location, nil
}

func (a *adapter) message(ctx context.Context, m sdk.Message, edited bool) (*domain.InboundEvent, error) {
	if m.Type != sdk.MessageTypeDefault && m.Type != sdk.MessageTypeReply {
		return nil, nil
	}
	if m.Author.ID == 0 || uint64(m.Author.ID) == a.self.Load() {
		return nil, nil
	}
	mentions := false
	for _, user := range m.Mentions {
		if uint64(user.ID) == a.self.Load() {
			mentions = true
		}
	}
	location, err := a.location(ctx, m.ChannelID)
	if err != nil {
		return nil, err
	}
	if location.thread != nil && !mentions &&
		(a.acceptConversation == nil || a.acceptConversation(location.parent, location.direct)) {
		joined, err := a.inThread(ctx, *location.thread)
		if err != nil {
			a.warnLookup(ctx, m.ChannelID, m.ID, err)
		} else if !joined {
			return nil, nil
		}
	}
	e := &domain.InboundEvent{SourceKey: a.name + ":m:" + m.ID.String(), Kind: domain.InboundEventMessage, Conversation: location.conversation, Parent: location.parent,
		MessageRef: Conversation(a.name, m.ChannelID.String(), m.ID.String()), Direct: location.direct, Sender: sender(m.Author, m.Member), Text: m.Content, MentionsMe: mentions, ReceivedAt: m.CreatedAt,
		Attachments: a.attachments(m)}
	if edited {
		if m.EditedTimestamp == nil {
			return nil, nil
		}
		e.Kind = domain.InboundEventMessageEdited
		e.ReceivedAt = *m.EditedTimestamp
		e.SourceKey = a.name + ":e:" + m.ID.String() + ":" + m.EditedTimestamp.UTC().Format(time.RFC3339Nano)
	}
	return e, nil
}

func (a *adapter) inThread(ctx context.Context, thread sdk.GuildThread) (bool, error) {
	if a.joinedThreads[thread.ID()] {
		return true, nil
	}
	self := snowflake.ID(a.self.Load())
	if thread.OwnerID == self {
		a.joinedThreads[thread.ID()] = true
		return true, nil
	}
	_, err := a.api.GetThreadMember(thread.ID(), self, false, rest.WithCtx(ctx))
	if err == nil {
		a.joinedThreads[thread.ID()] = true
		return true, nil
	}
	if !rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownMember) {
		return false, &lookupError{"GetThreadMember", err}
	}
	root, err := a.api.GetMessage(*thread.ParentID(), thread.ID(), rest.WithCtx(ctx))
	if rest.IsJSONErrorCode(err, rest.JSONErrorCodeUnknownMessage) {
		return false, nil
	}
	if err != nil {
		return false, &lookupError{"GetMessage", err}
	}
	if root.Author.ID == self {
		a.joinedThreads[thread.ID()] = true
		return true, nil
	}
	return false, nil
}

func (a *adapter) attachments(m sdk.Message) []domain.Attachment {
	var result []domain.Attachment
	for _, f := range m.Attachments {
		mime := ""
		if f.ContentType != nil {
			mime = *f.ContentType
		}
		result = append(result, domain.Attachment{Ref: fmt.Sprintf("%s:%s:%s:%s", a.name, m.ChannelID, m.ID, f.ID), Name: f.Filename, MIMEType: mime})
	}
	return result
}

func (a *adapter) deleted(ctx context.Context, channel, id snowflake.ID) (*domain.InboundEvent, error) {
	stored, err := a.history.GetInbound(ctx, Conversation(a.name, channel.String(), id.String()))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, &lookupError{"GetInbound", err}
	}
	stored.SourceKey = a.name + ":d:" + id.String()
	stored.Kind, stored.Deleted, stored.Text = domain.InboundEventMessageEdited, true, ""
	stored.Accepted, stored.Attachments, stored.MentionsMe = nil, nil, false
	stored.ReceivedAt = time.Now()
	return &stored, nil
}

func (a *adapter) reaction(ctx context.Context, e gateway.EventMessageReactionAdd) (*domain.InboundEvent, error) {
	if uint64(e.UserID) == a.self.Load() {
		return nil, nil
	}
	location, err := a.location(ctx, e.ChannelID)
	if err != nil {
		return nil, err
	}
	if !location.direct && e.MessageAuthorID == nil {
		message, err := a.api.GetMessage(e.ChannelID, e.MessageID, rest.WithCtx(ctx))
		if err != nil {
			return nil, &lookupError{"GetMessage", err}
		}
		e.MessageAuthorID = &message.Author.ID
	}
	if !location.direct && uint64(*e.MessageAuthorID) != a.self.Load() {
		return nil, nil
	}
	if a.acceptConversation != nil {
		target := location.conversation
		if location.parent != "" {
			target = location.parent
		}
		if !a.acceptConversation(target, location.direct) {
			return nil, nil
		}
	}
	var user sdk.User
	if e.Member != nil && e.Member.User.ID == e.UserID {
		user = e.Member.User
	} else {
		fetched, err := a.api.GetUser(e.UserID, rest.WithCtx(ctx))
		if err != nil {
			return nil, &lookupError{"GetUser", err}
		}
		user = *fetched
	}
	if e.Emoji.Name == nil {
		return nil, nil
	}
	emoji := *e.Emoji.Name
	if e.Emoji.ID != nil {
		emoji += ":" + e.Emoji.ID.String()
	}
	return &domain.InboundEvent{SourceKey: a.name + ":r:" + e.MessageID.String() + ":" + e.UserID.String() + ":" + emoji,
		Kind: domain.InboundEventReaction, Conversation: location.conversation, Parent: location.parent, MessageRef: Conversation(a.name, e.ChannelID.String(), e.MessageID.String()), Direct: location.direct,
		Sender: sender(user, e.Member), Text: fmt.Sprintf(":%s: on message %s", *e.Emoji.Name, e.MessageID), ReceivedAt: time.Now()}, nil
}

func (a *adapter) backfill(ctx context.Context, consume func(context.Context, domain.InboundEvent) error) error {
	conversations, err := a.history.ListConversations(ctx, a.name)
	if err != nil {
		a.warnLookup(ctx, 0, 0, &lookupError{"ListConversations", err})
	}
	owner, _ := parseID(a.config.Owner) // Validated by newAdapter.
	dm, err := a.api.CreateDMChannel(owner, rest.WithCtx(ctx))
	if err != nil {
		a.warnLookup(ctx, 0, 0, &lookupError{"CreateDMChannel", err})
	} else {
		conversations = append([]domain.Conversation{{Ref: Conversation(a.name, dm.ID().String(), ""), Direct: true}}, conversations...)
	}
	seen := make(map[domain.ConversationRef]bool)
	for _, conversation := range conversations {
		ref := conversation.Ref
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if ctx.Err() != nil {
			return nil
		}
		events, err := a.backfillChannel(ctx, ref)
		if err != nil {
			channel, _, _ := a.destination(ref)
			a.warnLookup(ctx, channel, 0, err)
			continue
		}
		for _, event := range events {
			if ctx.Err() != nil {
				return nil
			}
			if err := consume(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *adapter) backfillChannel(ctx context.Context, ref domain.ConversationRef) ([]domain.InboundEvent, error) {
	channel, _, err := a.destination(ref)
	if err != nil {
		return nil, err
	}
	location, err := a.location(ctx, channel)
	if err != nil {
		return nil, err
	}
	if location.thread != nil {
		return nil, nil
	}
	window := time.Now().Add(-24 * time.Hour)
	since := window
	last, err := a.history.LatestMessageTime(ctx, ref)
	if err != nil {
		return nil, &lookupError{"LatestMessageTime", err}
	}
	if last.After(since) {
		since = last
	}
	var events []domain.InboundEvent
	var before snowflake.ID
	for count := 0; count < 200; {
		page, err := a.api.GetMessages(channel, 0, before, 0, 100, rest.WithCtx(ctx))
		if err != nil {
			return nil, &lookupError{"GetMessages", err}
		}
		if len(page) == 0 {
			break
		}
		count += len(page)
		oldest := page[0].ID
		stop := false
		for _, m := range page {
			if m.ID < oldest {
				oldest = m.ID
			}
			if m.CreatedAt.Before(window) {
				stop = true
				continue
			}
			for _, edited := range []bool{false, true} {
				at := m.CreatedAt
				if edited {
					if m.EditedTimestamp == nil {
						continue
					}
					at = *m.EditedTimestamp
				}
				if at.Before(since) {
					continue
				}
				e, err := a.message(ctx, m, edited)
				if err != nil {
					a.warnLookup(ctx, m.ChannelID, m.ID, err)
					break
				}
				if e != nil {
					events = append(events, *e)
				}
			}
		}
		if stop || len(page) < 100 {
			break
		}
		if before != 0 && oldest >= before {
			return nil, errors.New("history cursor did not advance for Discord")
		}
		before = oldest
	}
	// Consume in `ReceivedAt` order. The next backfill starts at the newest stored message or edit,
	// so an edit stored before older unconsumed messages would make it skip them.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ReceivedAt.Equal(events[j].ReceivedAt) {
			if events[i].Kind != events[j].Kind {
				return events[i].Kind == domain.InboundEventMessage
			}
			return events[i].SourceKey < events[j].SourceKey
		}
		return events[i].ReceivedAt.Before(events[j].ReceivedAt)
	})
	return events, nil
}

// Lookup errors carry an operation without exposing REST response bodies in logs.
type lookupError struct {
	operation string
	err       error
}

func (e *lookupError) Error() string { return e.operation }
func (e *lookupError) Unwrap() error { return e.err }

func (a *adapter) warnLookup(ctx context.Context, channel, message snowflake.ID, err error) {
	if ctx.Err() != nil {
		return
	}
	operation := "decode event"
	var lookup *lookupError
	if errors.As(err, &lookup) {
		operation = lookup.operation
	}
	status := 0
	var response *rest.Error
	if errors.As(err, &response) && response.Response != nil {
		status = response.Response.StatusCode
	}
	slog.WarnContext(ctx, "Discord lookup failed", "adapter", a.name, "operation", operation,
		"channel_id", channel.String(), "message_id", message.String(), "http_status", status)
}
