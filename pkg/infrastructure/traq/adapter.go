package traq

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdk "github.com/traPtitech/go-traq"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/util/lifecycle"
)

type Config struct {
	Origin      string
	AccessToken string
	Owner       string
}

type adapter struct {
	name               string
	config             Config
	api                *sdk.APIClient
	http               *http.Client
	history            domain.EventRepository
	life               lifecycle.Group
	self               string
	started            time.Time
	users              sync.Map
	channels           sync.Map
	direct             sync.Map
	posted             sync.Map
	stamps             sync.Map
	acceptConversation func(domain.ConversationRef) bool
	snapshots          map[string]map[string]time.Time
}

func newAdapter(name string, config Config, history domain.EventRepository) (*adapter, error) {
	if !domain.ValidMessagingName(name) {
		return nil, errors.New("invalid messaging entry name")
	}
	if err := ValidateOrigin(config.Origin); err != nil {
		return nil, fmt.Errorf("traQ origin %w", err)
	}
	if config.AccessToken == "" {
		return nil, errors.New("access token is required for traQ")
	}
	if history == nil {
		return nil, errors.New("history repository is required for traQ")
	}
	config.Origin = strings.TrimRight(config.Origin, "/")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	settings := sdk.NewConfiguration()
	settings.Servers = sdk.ServerConfigurations{{URL: config.Origin + "/api/v3"}}
	settings.DefaultHeader["Authorization"] = "Bearer " + config.AccessToken
	settings.HTTPClient = client
	return &adapter{name: name, config: config, api: sdk.NewAPIClient(settings), http: client, history: history, snapshots: make(map[string]map[string]time.Time)}, nil
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validID(id string) bool { return idPattern.MatchString(id) }

func Conversation(name, channel, message string) domain.ConversationRef {
	return domain.NewConversationRef(name, channel, message)
}

func (a *adapter) PostRefs(_, message domain.ConversationRef, direct bool) []domain.NamedRef {
	if direct || message == "" {
		return nil
	}
	return []domain.NamedRef{{Name: "quote", Ref: message}}
}

func (a *adapter) destination(ref domain.ConversationRef) (string, string, error) {
	name, channel, message, ok := domain.ParseConversationRef(ref)
	if !ok || name != a.name || !validID(channel) || (message != "" && !validID(message)) {
		return "", "", errors.New("invalid traQ conversation reference")
	}
	return channel, message, nil
}

func (a *adapter) messageText(ref domain.ConversationRef, message domain.OutboundMessage) (string, string, error) {
	channel, quote, err := a.destination(ref)
	if err != nil {
		return "", "", err
	}
	text := message.Text
	for _, f := range message.Attachments {
		if !validID(f.Ref) {
			return "", "", errors.New("file_id must be a bare traQ file ID")
		}
		text += "\n" + a.config.Origin + "/files/" + f.Ref
	}
	if quote != "" {
		text += "\n" + a.config.Origin + "/messages/" + quote
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 10000 {
		return "", "", errors.New("traQ messages must be valid UTF-8 and at most 10000 characters, including file and quote URLs")
	}
	if strings.TrimSpace(text) == "" {
		return "", "", errors.New("traQ message must not be empty")
	}
	return channel, text, nil
}

func (a *adapter) Send(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	channel, text, err := a.messageText(ref, message)
	if err != nil {
		return "", err
	}
	request := sdk.NewPostMessageRequest(text)
	request.SetEmbed(true)
	result, response, err := a.api.MessageAPI.PostMessage(ctx, channel).PostMessageRequest(*request).Execute()
	if err != nil {
		return "", apiError("post traQ message", response, err)
	}
	if !validID(result.Id) {
		return "", errors.New("traQ returned an invalid message ID")
	}
	a.posted.Store(result.Id, channel)
	return result.Id, nil
}

func (a *adapter) Shutdown(ctx context.Context) error { return a.life.Shutdown(ctx) }

func (a *adapter) DirectMessage(ctx context.Context, user string) (domain.ConversationRef, error) {
	if !validID(user) {
		return "", errors.New("user must be a bare traQ user ID")
	}
	result, response, err := a.api.UserAPI.GetUserDMChannel(ctx, user).Execute()
	if err != nil {
		return "", apiError("resolve traQ direct message", response, err)
	}
	if !validID(result.Id) {
		return "", errors.New("traQ returned an invalid direct message channel ID")
	}
	a.direct.Store(result.Id, user)
	return Conversation(a.name, result.Id, ""), nil
}

func (a *adapter) SetConversationFilter(accept func(domain.ConversationRef) bool) {
	a.acceptConversation = accept
}

func (a *adapter) UserGroups(ctx context.Context, ids []string) (map[string][]string, error) {
	groups := make(map[string][]string, len(ids))
	for _, id := range ids {
		if !validID(id) {
			return nil, errors.New("group must be a bare traQ group ID")
		}
		members, response, err := a.api.GroupAPI.GetUserGroupMembers(ctx, id).Execute()
		if err != nil {
			return nil, apiError("read traQ group", response, err)
		}
		groups[id] = make([]string, 0, len(members))
		for _, member := range members {
			groups[id] = append(groups[id], member.Id)
		}
	}
	return groups, nil
}
