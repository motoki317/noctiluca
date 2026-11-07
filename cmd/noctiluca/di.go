package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/slack-go/slack"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/claude"
	"github.com/motoki317/noctiluca/pkg/infrastructure/mcpclient"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/motoki317/noctiluca/pkg/infrastructure/repository"
	"github.com/motoki317/noctiluca/pkg/infrastructure/slack"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/motoki317/noctiluca/pkg/usecase/core"
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
	hub, err := toolhub.New(toolhub.Config{Port: port})
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
	store, err := memory.New(ctx, memory.Config{Path: config.Memory.Dir, Defaults: consolidate.Procedures})
	if err != nil {
		return nil, err
	}
	episodes := repository.NewEpisodeRepository(db)
	if err := hub.AddSource("consolidate", []domain.Tool{consolidate.EpisodesTool(episodes, config.Consolidate.NegativeReactions)}); err != nil {
		return nil, err
	}
	events := repository.NewEventRepository(db)
	messaging, err := slack.New("slack", tokens, events, slackOptions...)
	if err != nil {
		return nil, err
	}
	external, err := mcpclient.New(config.MCPServers, repository.NewOAuthTokenRepository(db), repository.NewInboxRepository(db), hub, logger)
	if err != nil {
		return nil, err
	}
	rules := make(map[domain.ConversationRef]domain.AcceptRule, len(config.Conversations.Rules))
	for _, r := range config.Conversations.Rules {
		rules[slack.Conversation("slack", r.ID, "")] = r.rule()
	}
	direct := domain.AcceptRule{Users: []string{config.Owner}}
	if config.Conversations.DirectMessages != nil {
		direct = config.Conversations.DirectMessages.rule()
	}

	model := claude.New(config.Model.Executable, config.Model.Name)
	jobs := append([]domain.Schedule(nil), config.Cron...)
	if config.Heartbeat.Enabled {
		jobs = append(jobs, domain.Schedule{Name: "heartbeat", Cron: "@every " + config.Heartbeat.Interval.String(), TZ: "UTC", Workdir: config.Model.Workdir})
	}
	if config.Consolidate.Enabled {
		jobs = append(jobs, domain.Schedule{Name: "consolidate", Cron: config.Consolidate.Cron, TZ: config.Consolidate.TZ, Workdir: config.Model.Workdir})
	}
	for name, enabled := range map[string]bool{"heartbeat": config.Heartbeat.Enabled, "consolidate": config.Consolidate.Enabled} {
		if !enabled {
			if err := repository.NewScheduleRepository(db).Delete(ctx, name); err != nil {
				return nil, err
			}
		}
	}
	procedure := func(ctx context.Context, name string) (string, error) {
		text, err := store.Read(ctx, "procedures/"+name+".md")
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(text), err
	}
	for i := range jobs {
		if jobs[i].Workdir == "" {
			jobs[i].Workdir = config.Model.Workdir
		}
	}
	agent, err := core.New(core.Config{
		IdleTimeout: config.Model.IdleTimeout,
		Workdir:     config.Model.Workdir,
		Jobs:        jobs, Procedure: procedure, TaskTimeout: config.Tasks.Timeout, TurnTimeout: config.Model.TurnTimeout,
		Completed: consolidate.Checkpoint(episodes),
	}, []core.Messaging{{Name: "slack", Adapter: messaging, Owner: config.Owner, Rules: rules,
		DirectMessages: &direct, GroupRefreshInterval: config.Slack.GroupRefreshInterval}}, model, hub, events, repository.NewSessionRepository(db), nil,
		repository.NewScheduleRepository(db), repository.NewWatchRepository(db), external, store, repository.NewInboxRepository(db))
	if err != nil {
		return nil, err
	}
	return &application{core: agent, db: db}, nil
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
