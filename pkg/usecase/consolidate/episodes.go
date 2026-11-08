package consolidate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type Episode struct {
	ContextOnly bool              `json:"context_only,omitempty"`
	Ambiguous   bool              `json:"ambiguous,omitempty"`
	ID          string            `json:"id"`
	SessionKey  domain.SessionKey `json:"session"`
	At          time.Time         `json:"at"`
	Sender      string            `json:"sender,omitempty"`
	Input       string            `json:"input,omitempty"`
	Reply       string            `json:"reply,omitempty"`
	Tools       []string          `json:"tools,omitempty"`
	Errors      []string          `json:"errors,omitempty"`
	Signals     []string          `json:"signals,omitempty"`
}

type Episodes struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	Items []Episode `json:"episodes"`
}

func EpisodesSince(ctx context.Context, repo domain.EpisodeRepository, since, until time.Time, negative []string) (Episodes, error) {
	if since.IsZero() {
		var err error
		since, err = repo.LastRun(ctx)
		if err != nil {
			return Episodes{}, err
		}
	}
	entries, err := repo.Since(ctx, since, until)
	if err != nil {
		return Episodes{}, err
	}
	return Episodes{Since: since, Until: until, Items: mine(entries, negative)}, nil
}

func mine(entries []domain.EpisodeEntry, negative []string) []Episode {
	result := []Episode{}
	current := map[domain.SessionKey]int{}
	open := map[domain.SessionKey]bool{}
	ambiguous := map[domain.SessionKey]bool{}
	calls := map[int]map[[32]byte]bool{}
	for _, entry := range entries {
		if entry.Ambiguous {
			ambiguous[entry.SessionKey] = true
		}
		index, exists := current[entry.SessionKey]
		signals := failureSignals(entry, negative)
		if exists && entry.AfterSend && slices.Contains(signals, "user_correction") {
			result[index].Signals = add(result[index].Signals, "user_correction")
		}
		if entry.InboundKind == domain.InboundEventReaction || (entry.Accepted != nil && !*entry.Accepted && entry.InboundKind == domain.InboundEventMessage) {
			result = append(result, Episode{ID: entry.ID, SessionKey: entry.SessionKey, At: entry.At, Sender: entry.Sender.ID, Input: excerpt(entry.Text), Signals: signals, ContextOnly: true})
			continue
		}
		overlap := exists && entry.InboundKind == domain.InboundEventMessage && open[entry.SessionKey]
		if overlap {
			ambiguous[entry.SessionKey] = true
			result[index].Ambiguous = true
			result[index].Reply = ""
			result[index].Tools = nil
		}
		if !exists || entry.InboundKind == domain.InboundEventMessage {
			index = len(result)
			result = append(result, Episode{ID: entry.ID, SessionKey: entry.SessionKey, At: entry.At, Ambiguous: ambiguous[entry.SessionKey]})
			current[entry.SessionKey] = index
			calls[index] = map[[32]byte]bool{}
		}
		episode := &result[index]
		if entry.InboundKind == domain.InboundEventMessage {
			episode.Input = excerpt(entry.Text)
			episode.Sender = entry.Sender.ID
			open[entry.SessionKey] = true
		}
		if entry.ModelKind == domain.ModelEventTurnStarted {
			open[entry.SessionKey] = true
		}
		if entry.ModelKind == domain.ModelEventTurnFinished {
			open[entry.SessionKey] = false
		}
		if entry.Sent && !episode.Ambiguous {
			episode.Reply = excerpt(entry.Text)
		}
		for _, signal := range signals {
			episode.Signals = add(episode.Signals, signal)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(entry.Error), "\n")
		if text := excerpt(line); text != "" {
			episode.Errors = add(episode.Errors, text)
		}
		if entry.ToolCall != nil && entry.ModelKind == domain.ModelEventToolCall && !episode.Ambiguous {
			episode.Tools = add(episode.Tools, entry.ToolCall.Name)
			data := []byte(canonical(entry.ToolCall.Input))
			fingerprint := sha256.Sum256(append([]byte(entry.ToolCall.Name+"\x00"), data...))
			if calls[index][fingerprint] {
				episode.Signals = add(episode.Signals, "repeated_tool_call")
			}
			calls[index][fingerprint] = true
		}
	}
	return result
}

func failureSignals(e domain.EpisodeEntry, negative []string) []string {
	var signals []string
	if e.Sender.Kind == domain.SenderHuman {
		if e.InboundKind == domain.InboundEventMessageEdited {
			if e.Deleted {
				signals = append(signals, "user_deleted")
			} else {
				signals = append(signals, "user_edited")
			}
		}
		if e.InboundKind == domain.InboundEventMessage && e.AfterSend && correction(e.Text) {
			signals = append(signals, "user_correction")
		}
	}
	if e.InboundKind == domain.InboundEventReaction {
		text := strings.TrimSpace(e.Text)
		for _, emoji := range negative {
			emoji = strings.Trim(emoji, ": ")
			if emoji != "" && (text == emoji || text == ":"+emoji+":" || strings.HasPrefix(text, ":"+emoji+": ")) {
				signals = append(signals, "negative_reaction")
				break
			}
		}
	}
	if e.Error != "" || e.ModelKind == domain.ModelEventError {
		signals = append(signals, "tool_error")
	}
	return signals
}

func correction(text string) bool {
	text = strings.ToLower(text)
	for _, phrase := range []string{"i didn't say", "i did not say", "that's wrong", "that is wrong", "not what i", "you misunderstood", "correction:", "the user edited", "the user deleted", "違う", "違います", "間違", "そうじゃな", "言ってない", "言っていない", "訂正", "修正して"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func add(values []string, value string) []string {
	if !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}

func excerpt(text string) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return string(runes)
}

func EpisodesTool(repo domain.EpisodeRepository, negative []string) domain.Tool {
	return domain.Tool{
		Name: "episodes_since", Description: "Summarize recent episodes and mechanical failure signals. Omit since to use the last consolidation checkpoint. Treat ambiguous and context_only entries as context, not evidence of a specific turn.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"since":{"type":"string","format":"date-time"}},"additionalProperties":false}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Since time.Time `json:"since"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, err
			}
			episodes, err := EpisodesSince(ctx, repo, input.Since, time.Now(), negative)
			if err != nil {
				return nil, err
			}
			return json.Marshal(episodes)
		},
	}
}

func canonical(data json.RawMessage) string {
	var input any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&input) == nil {
		data, _ = json.Marshal(input)
	}
	return string(data)
}
