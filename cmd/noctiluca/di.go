package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	sdk "github.com/slack-go/slack"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/claude"
	"github.com/motoki317/noctiluca/pkg/infrastructure/codex"
	"github.com/motoki317/noctiluca/pkg/infrastructure/discord"
	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/infrastructure/traq"
	"github.com/motoki317/noctiluca/pkg/usecase/core"
	memoryusecase "github.com/motoki317/noctiluca/pkg/usecase/memory"
	"github.com/motoki317/noctiluca/pkg/usecase/toolhub"
)

type application struct {
	core      domain.Core
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

func newApplication(ctx context.Context, config Config, logger *slog.Logger, slackOptions ...sdk.Option) (_ *application, result error) {
	tokens, port, err := config.validateRun()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.Model.Workdir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(config.Model.Workdir, 0700); err != nil {
		return nil, err
	}
	db, err := repository.New(ctx, config.Database)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, db.Close())
		}
	}()
	hubConfig := toolhub.Config{Port: port}
	var model domain.ModelAdapter
	if config.Model.Type == "codex" {
		model = codex.New(config.Model.Executable, config.Model.Name, config.Model.ReasoningEffort)
	} else {
		model = claude.New(config.Model.Executable, config.Model.Name)
		hubConfig.AlwaysOfferedMeta = claude.AlwaysOfferedToolMeta
	}
	hub, err := toolhub.New(hubConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result = errors.Join(result, hub.Shutdown(stopCtx))
		}
	}()
	store, err := memory.New(ctx, memory.Config{Path: config.Memory.Dir})
	if err != nil {
		return nil, err
	}
	events := repository.NewEventRepository(db)
	messaging := make([]core.Messaging, 0, len(config.Messaging))
	for i, instance := range config.Messaging {
		trust := instance.Trust.rule()
		rules := make(map[domain.ConversationRef]domain.AcceptRule, len(instance.Conversations))
		for _, r := range instance.Conversations {
			rules[domain.NewConversationRef(instance.Name, r.ID, "")] = r.rule(instance.Trust)
		}
		var adapter domain.MessagingAdapter
		switch instance.Type {
		case "slack":
			slackConfig := tokens[i].Slack
			slackConfig.Owner = instance.Owner
			adapter, err = slack.New(instance.Name, slackConfig, events, slackOptions...)
		case "discord":
			adapter, err = discord.New(instance.Name, discord.Config{Token: tokens[i].DiscordToken, Owner: instance.Owner, NeedGroups: needsGroups(trust, rules)}, events)
		case "traq":
			adapter, err = traq.New(instance.Name, traq.Config{Origin: instance.TraQ.Origin, AccessToken: tokens[i].TraQAccessToken, Owner: instance.Owner}, events)
		}
		if err != nil {
			return nil, fmt.Errorf("messaging.%s: %w", instance.Name, err)
		}
		defer func() {
			if result != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result = errors.Join(result, adapter.Shutdown(stopCtx))
			}
		}()
		messaging = append(messaging, core.Messaging{Name: instance.Name, Adapter: adapter, Owner: instance.Owner, Rules: rules,
			Trust: trust, GroupRefreshInterval: instance.groupRefreshInterval()})
	}
	external, err := mcpclient.New(config.MCPServers, repository.NewOAuthTokenRepository(db), repository.NewInboxRepository(db), hub, logger)
	if err != nil {
		return nil, err
	}

	var jobs []domain.Schedule
	for _, job := range config.Cron {
		jobs = append(jobs, domain.Schedule{Name: job.Name, Cron: job.Cron, TZ: job.TZ, Prompt: job.Prompt, Workdir: job.Workdir})
	}
	if config.Heartbeat.Enabled {
		jobs = append(jobs, domain.Schedule{Name: domain.HeartbeatSchedule, Cron: "@every " + config.Heartbeat.Interval.String(), TZ: "UTC"})
	}
	if config.Rest.Enabled {
		jobs = append(jobs, domain.Schedule{Name: domain.RestSchedule, Cron: config.Rest.Cron, TZ: config.Rest.TZ})
	}
	// Schedules persist in SQLite, so a schedule from an earlier run keeps firing until it is deleted.
	for name, enabled := range map[string]bool{domain.HeartbeatSchedule: config.Heartbeat.Enabled, domain.RestSchedule: config.Rest.Enabled} {
		if !enabled {
			if err := repository.NewScheduleRepository(db).Delete(ctx, name); err != nil {
				return nil, err
			}
		}
	}
	agent, err := core.New(core.Config{
		Workdir: config.Model.Workdir,
		Jobs:    jobs, Procedure: memoryusecase.Procedure,
	}, messaging, model, hub, events, repository.NewSessionRepository(db), nil,
		repository.NewScheduleRepository(db), repository.NewWatchRepository(db), external, store, repository.NewInboxRepository(db))
	if err != nil {
		return nil, err
	}
	return &application{core: agent, db: db}, nil
}

func needsGroups(entry domain.AcceptRule, overrides map[domain.ConversationRef]domain.AcceptRule) bool {
	if len(entry.Groups) > 0 {
		return true
	}
	for _, rule := range overrides {
		if len(rule.Groups) > 0 {
			return true
		}
	}
	return false
}

func (a *application) Start(ctx context.Context) (result error) {
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result = errors.Join(result, a.Shutdown(shutdownCtx))
	}()
	// Core owns the service errgroup and joins model processes before SQLite closes.
	return a.core.Start(ctx)
}

func (a *application) Shutdown(ctx context.Context) error {
	if err := a.core.Shutdown(ctx); err != nil {
		return err
	}
	a.closeOnce.Do(func() { a.closeErr = a.db.Close() })
	return a.closeErr
}
