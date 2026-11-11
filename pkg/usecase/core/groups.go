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
	r.Bots = slices.Clone(r.Bots)
	return r
}

type compiledRules struct {
	conversations map[domain.ConversationRef]rule
	entry         rule
}

func newMessagingInstance(config Messaging) (*messagingInstance, error) {
	if config.GroupRefreshInterval < 0 {
		return nil, errors.New("group refresh interval must be positive")
	}
	if config.GroupRefreshInterval == 0 {
		config.GroupRefreshInterval = time.Hour
	}
	config.Rules = cloneRules(config.Rules)
	config.Trust = cloneRule(config.Trust)
	config.UserGroups = cloneGroups(config.UserGroups)
	all := []domain.AcceptRule{config.Trust}
	for _, r := range config.Rules {
		all = append(all, r)
	}
	var ids []string
	for _, r := range all {
		for _, id := range r.Groups {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	if _, ok := config.Adapter.(domain.UserGroupProvider); ok {
		for _, id := range ids {
			if _, exists := config.UserGroups[id]; !exists {
				config.UserGroups[id] = nil
			}
		}
	}
	rules, err := compileRules(config)
	if err != nil {
		return nil, err
	}
	return &messagingInstance{config: config, rules: rules, groupIDs: ids}, nil
}

func compileRules(config Messaging) (compiledRules, error) {
	rules := compiledRules{conversations: make(map[domain.ConversationRef]rule, len(config.Rules))}
	entry, err := compileRule(config, config.Trust)
	if err != nil {
		return rules, err
	}
	rules.entry = entry
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
	return rules, nil
}

func compileRule(config Messaging, value domain.AcceptRule) (rule, error) {
	r := rule{users: make(map[string]struct{}), bots: make(map[string]struct{}), anySender: value.AnySender, ignore: value.Ignore, requireMention: value.RequireMention}
	users := slices.Clone(value.Users)
	if config.Owner != "" {
		users = append(users, config.Owner)
	}
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
	for _, bot := range value.Bots {
		if bot == "" {
			return r, errors.New("accepted bot ID must not be empty")
		}
		r.bots[bot] = struct{}{}
	}
	return r, nil
}

func (c *core) findRule(conversation domain.ConversationRef) (rule, bool) {
	instance, err := c.messaging.resolve(conversation)
	if err != nil {
		return rule{}, false
	}
	rules := instance.rules
	r, ok := rules.conversations[conversation]
	if !ok {
		r = rules.entry
	}
	return r, !r.ignore
}

func (c *core) applyGroups(instance *messagingInstance, groups map[string][]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	config := instance.config
	config.UserGroups = cloneGroups(groups)
	rules, err := compileRules(config)
	if err != nil {
		return err
	}
	instance.config.UserGroups = config.UserGroups
	instance.rules = rules
	c.reconciler.Notify()
	return nil
}

func (c *core) loadGroups(ctx context.Context, instance *messagingInstance, provider domain.UserGroupProvider) {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	groups, err := provider.UserGroups(refreshCtx, instance.groupIDs)
	if err == nil && refreshCtx.Err() == nil {
		err = c.applyGroups(instance, groups)
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "User group refresh failed; retaining previous membership", "messaging_entry", instance.config.Name, "error", err)
	}
}

func (c *core) refreshGroups(ctx context.Context, instance *messagingInstance, provider domain.UserGroupProvider) error {
	ticker := time.NewTicker(instance.config.GroupRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.loadGroups(ctx, instance, provider)
		}
	}
}
