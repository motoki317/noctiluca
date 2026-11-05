package reconcile

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/robfig/cron/v3"
)

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
