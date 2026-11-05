package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/robfig/cron/v3"
)

func ValidateCron(expression string) error {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return errors.New("cron requires exactly five fields")
	}
	_, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(strings.Join(fields, " "))
	if err != nil {
		return fmt.Errorf("cron requires exactly five fields: %w", err)
	}
	return nil
}

func Next(s domain.Schedule, now time.Time) (time.Time, error) {
	if s.Name == "" || s.TZ == "" {
		return time.Time{}, errors.New("schedule name and timezone are required")
	}
	if _, err := time.LoadLocation(s.TZ); err != nil {
		return time.Time{}, err
	}
	parsed, err := cron.ParseStandard("CRON_TZ=" + s.TZ + " " + s.Cron)
	if err != nil {
		return time.Time{}, err
	}
	next := parsed.Next(now)
	if next.IsZero() {
		return next, errors.New("schedule has no future occurrence")
	}
	return next, nil
}
func TaskItem(name, prompt, workdir string) domain.InboxItem {
	return domain.InboxItem{SourceKey: "task:" + uuid.NewString(), SessionKey: domain.SessionKey("task:" + name), Actor: domain.ActorOwner, Lane: "task", ReadyAt: time.Now(), Payload: domain.InboxPayload{Input: domain.UserInput{Text: prompt}, Context: domain.SessionContext{Actor: domain.ActorOwner, TaskName: name, Workdir: workdir}}}
}
func (r *Reconciler) Install(ctx context.Context, schedules []domain.Schedule) error {
	for _, s := range schedules {
		if s.Workdir == "" {
			s.Workdir = r.config.Workdir
		}
		next, err := Next(s, time.Now())
		if err != nil {
			return fmt.Errorf("schedule %s: %w", s.Name, err)
		}
		s.NextRun = next
		s.Actor = domain.ActorOwner
		if err := r.schedules.Save(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
func (r *Reconciler) Tools() []domain.Tool {
	return []domain.Tool{
		{Name: "schedule_add", OwnerOnly: true, Description: "Add or update a cron task. Use five cron fields and an IANA timezone.", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"cron":{"type":"string"},"tz":{"type":"string"},"prompt":{"type":"string"},"workdir":{"type":"string"}},"required":["name","cron","tz","prompt"],"additionalProperties":false}`), Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var s domain.Schedule
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, err
			}
			if err := ValidateCron(s.Cron); err != nil {
				return nil, err
			}
			if s.Name == "heartbeat" || s.Name == "consolidate" || strings.TrimSpace(s.Prompt) == "" {
				return nil, errors.New("reserved task name or empty prompt")
			}
			if err := r.Install(ctx, []domain.Schedule{s}); err != nil {
				return nil, err
			}
			r.Notify()
			return json.Marshal(map[string]string{"name": s.Name})
		}},
		{Name: "schedule_remove", OwnerOnly: true, Description: "Remove a cron task. Config tasks return on restart.", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`), Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var in struct{ Name string }
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if err := r.schedules.Delete(ctx, in.Name); err != nil {
				return nil, err
			}
			r.Notify()
			return json.Marshal(map[string]string{"name": in.Name})
		}},
	}
}
