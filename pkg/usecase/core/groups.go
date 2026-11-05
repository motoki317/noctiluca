package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func cloneGroups(groups map[string][]string) map[string][]string {
	copy := make(map[string][]string, len(groups))
	for id, users := range groups {
		copy[id] = slices.Clone(users)
	}
	return copy
}
func cloneRules(rules map[domain.ConversationRef]domain.AcceptRule) map[domain.ConversationRef]domain.AcceptRule {
	copy := make(map[domain.ConversationRef]domain.AcceptRule, len(rules))
	for ref, r := range rules {
		copy[ref] = cloneRule(r)
	}
	return copy
}

func cloneRule(r domain.AcceptRule) domain.AcceptRule {
	r.Users = slices.Clone(r.Users)
	r.Groups = slices.Clone(r.Groups)
	return r
}

type compiledRules struct {
	conversations map[domain.ConversationRef]rule
	direct        *rule
}

func compileRules(config Config) (compiledRules, error) {
	rules := compiledRules{conversations: make(map[domain.ConversationRef]rule, len(config.Rules))}
	for conversation, value := range config.Rules {
		if conversation == "" {
			return rules, errors.New("accept rule conversation is required")
		}
		r, err := compileRule(config, value)
		if err != nil {
			return rules, err
		}
		rules.conversations[conversation] = r
	}
	if config.DirectMessages != nil {
		r, err := compileRule(config, *config.DirectMessages)
		if err != nil {
			return rules, err
		}
		rules.direct = &r
	}
	return rules, nil
}

func compileRule(config Config, value domain.AcceptRule) (rule, error) {
	r := rule{users: make(map[string]struct{}), allowBots: value.AllowBots, requireMention: value.RequireMention, actor: domain.ActorMember}
	users := slices.Clone(value.Users)
	for _, group := range value.Groups {
		members, ok := config.UserGroups[group]
		if !ok {
			return r, fmt.Errorf("unknown user group %q", group)
		}
		users = append(users, members...)
	}
	for _, user := range users {
		if user == "" {
			return r, errors.New("accepted user ID must not be empty")
		}
		r.users[user] = struct{}{}
	}
	if _, owner := r.users[config.Owner]; owner && len(r.users) == 1 {
		r.actor = domain.ActorOwner
	}
	return r, nil
}

func (c *core) findRule(rules compiledRules, conversation, parent domain.ConversationRef) (rule, bool) {
	r, ok := rules.conversations[conversation]
	if !ok && parent != "" {
		r, ok = rules.conversations[parent]
	}
	if !ok && rules.direct != nil {
		if provider, supported := c.messaging.(domain.DirectConversationProvider); supported && provider.DirectConversation(conversation) {
			return *rules.direct, true
		}
	}
	return r, ok
}

func (c *core) UpdateUserGroups(ctx context.Context, groups map[string][]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.applyGroups(groups)
}
func (c *core) applyGroups(groups map[string][]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	config := c.config
	config.UserGroups = cloneGroups(groups)
	rules, err := compileRules(config)
	if err != nil {
		return err
	}
	c.config.UserGroups = config.UserGroups
	c.rules = rules
	c.reconciler.Notify()
	return nil
}

func (c *core) refreshGroups(ctx context.Context, provider domain.UserGroupProvider) error {
	ticker := time.NewTicker(c.config.GroupRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			groups, err := provider.UserGroups(refreshCtx, c.groupIDs)
			if err == nil {
				err = c.UpdateUserGroups(refreshCtx, groups)
			}
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "User group refresh failed; retaining previous membership")
			}
		}
	}
}
