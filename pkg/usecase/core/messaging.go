package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type Messaging struct {
	Name                 string
	Adapter              domain.MessagingAdapter
	Owner                string
	Rules                map[domain.ConversationRef]domain.AcceptRule
	DirectMessages       *domain.AcceptRule
	UserGroups           map[string][]string
	GroupRefreshInterval time.Duration
}

type messagingInstance struct {
	config   Messaging
	rules    compiledRules
	groupIDs []string
}

type messagingRouter struct {
	instances []*messagingInstance
	byName    map[string]*messagingInstance
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func newMessagingRouter(configs []Messaging) (*messagingRouter, error) {
	if len(configs) == 0 {
		return nil, errors.New("at least one messaging entry is required")
	}
	router := &messagingRouter{byName: make(map[string]*messagingInstance, len(configs))}
	for _, config := range configs {
		if !instanceName.MatchString(config.Name) {
			return nil, fmt.Errorf("invalid messaging entry name %q", config.Name)
		}
		if _, exists := router.byName[config.Name]; exists {
			return nil, fmt.Errorf("duplicate messaging entry %q", config.Name)
		}
		if config.Adapter == nil {
			return nil, fmt.Errorf("messaging entry %q requires an adapter", config.Name)
		}
		instance, err := newMessagingInstance(config)
		if err != nil {
			return nil, fmt.Errorf("messaging entry %q: %w", config.Name, err)
		}
		router.instances = append(router.instances, instance)
		router.byName[config.Name] = instance
	}
	for _, instance := range router.instances {
		for ref := range instance.config.Rules {
			owner, err := router.resolve(ref)
			if err != nil {
				return nil, err
			}
			if owner != instance {
				return nil, fmt.Errorf("accept rule %q does not belong to messaging entry %q", ref, instance.config.Name)
			}
		}
	}
	return router, nil
}

func (r *messagingRouter) resolve(ref domain.ConversationRef) (*messagingInstance, error) {
	name, _, ok := strings.Cut(string(ref), ":")
	if instance, exists := r.byName[name]; ok && exists {
		return instance, nil
	}
	return nil, fmt.Errorf("unknown messaging entry in ref %q", ref)
}

func (r *messagingRouter) Send(ctx context.Context, ref domain.ConversationRef, message domain.OutboundMessage) (string, error) {
	instance, err := r.resolve(ref)
	if err != nil {
		return "", err
	}
	return instance.config.Adapter.Send(ctx, ref, message)
}

func (r *messagingRouter) Shutdown(ctx context.Context) (result error) {
	for _, instance := range r.instances {
		result = errors.Join(result, instance.config.Adapter.Shutdown(ctx))
	}
	return result
}
