package consolidate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/stretchr/testify/require"
)

func TestFailureSignals(t *testing.T) {
	for _, tc := range []struct {
		name, text, toolError string
		kind                  domain.InboundEventKind
		human, after, deleted bool
		want                  []string
	}{
		{name: "English correction", text: "I didn't say stop", kind: domain.InboundEventMessage, human: true, after: true, want: []string{"user_correction"}},
		{name: "not immediate", text: "訂正", kind: domain.InboundEventMessage, human: true},
		{name: "bot", text: "that's wrong", kind: domain.InboundEventMessage, after: true},
		{name: "ordinary", text: "Please continue", kind: domain.InboundEventMessage, human: true, after: true},
		{name: "edited", kind: domain.InboundEventMessageEdited, human: true, want: []string{"user_edited"}},
		{name: "deleted", kind: domain.InboundEventMessageEdited, human: true, deleted: true, want: []string{"user_deleted"}},
		{name: "negative reaction", kind: domain.InboundEventReaction, text: ":thumbsdown: on message 1", human: true, want: []string{"negative_reaction"}},
		{name: "positive reaction", kind: domain.InboundEventReaction, text: ":thumbsup: on message 1", human: true},
		{name: "tool error", toolError: "read failed", want: []string{"tool_error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind := domain.SenderBot
			if tc.human {
				kind = domain.SenderHuman
			}
			got := failureSignals(domain.EpisodeEntry{InboundKind: tc.kind, Text: tc.text, Sender: domain.Sender{Kind: kind}, AfterSend: tc.after, Deleted: tc.deleted, Error: tc.toolError}, []string{"thumbsdown", "no_entry"})
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMineCompactsEpisodesAndCanonicalizesCalls(t *testing.T) {
	entries := []domain.EpisodeEntry{
		{ID: "event:1", SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: strings.Repeat("日", 300), Sender: domain.Sender{ID: "owner", Kind: domain.SenderHuman}},
		{SessionKey: "room", ModelKind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{ID: "a", Name: "read", Input: json.RawMessage(`{"a":1,"b":2}`)}},
		{SessionKey: "room", ModelKind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{ID: "b", Name: "read", Input: json.RawMessage(`{ "b":2, "a":1 }`)}},
		{SessionKey: "room", Sent: true, Text: "Done"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnFinished},
		{ID: "event:2", SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "That's wrong", AfterSend: true, Sender: domain.Sender{Kind: domain.SenderHuman}},
		{SessionKey: "room", ModelKind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{Name: "read", Input: json.RawMessage(`{"a":1,"b":2}`)}},
	}
	got := mine(entries, nil)
	require.Len(t, got, 2)
	require.Len(t, []rune(got[0].Input), 201)
	require.Equal(t, []string{"repeated_tool_call", "user_correction"}, got[0].Signals)
	require.Equal(t, []string{"read"}, got[0].Tools)
	require.Equal(t, "Done", got[0].Reply)
	require.Equal(t, []string{"user_correction"}, got[1].Signals)
}

func TestMineErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        domain.ModelEventKind
		errors      []string
		want        []string
		wantSignals []string
	}{
		{
			name: "distinct summaries in appearance order",
			kind: domain.ModelEventToolResult,
			errors: []string{
				" \tread   failed \r\nfile not found",
				"write failed\npermission denied",
				" \tread   failed \r\nfile not found",
				"read failed\nanother file not found",
			},
			want:        []string{"read failed … file not found", "write failed … permission denied", "read failed … another file not found"},
			wantSignals: []string{"tool_error"},
		},
		{
			name:        "one line",
			kind:        domain.ModelEventToolResult,
			errors:      []string{"file not found"},
			want:        []string{"file not found"},
			wantSignals: []string{"tool_error"},
		},
		{
			name:        "same first and last non-empty lines",
			kind:        domain.ModelEventToolResult,
			errors:      []string{" \t\nread failed \r\nmore details\n\tread failed\n \t\r\n"},
			want:        []string{"read failed"},
			wantSignals: []string{"tool_error"},
		},
		{
			name:        "cap after joining lines",
			kind:        domain.ModelEventToolResult,
			errors:      []string{strings.Repeat("日", 190) + "\n" + strings.Repeat("界", 20)},
			want:        []string{strings.Repeat("日", 190) + " … " + strings.Repeat("界", 7) + "…"},
			wantSignals: []string{"tool_error"},
		},
		{
			name:   "empty tool result",
			kind:   domain.ModelEventToolResult,
			errors: []string{""},
		},
		{
			name:        "empty model error",
			kind:        domain.ModelEventError,
			errors:      []string{""},
			wantSignals: []string{"tool_error"},
		},
		{
			name: "blank first and last lines",
			kind: domain.ModelEventToolResult,
			errors: []string{
				" \t\nfile not found\nmore details\n \t\n",
				"\r\n \t\r\n permission  denied \r\nmore   details\r\n\t\r\n",
				"\nfile not found\nother details",
				" \t",
			},
			want:        []string{"file not found … more details", "permission denied … more details", "file not found … other details"},
			wantSignals: []string{"tool_error"},
		},
		{
			name:        "model error text",
			kind:        domain.ModelEventError,
			errors:      []string{"turn failed\nconnection lost"},
			want:        []string{"turn failed … connection lost"},
			wantSignals: []string{"tool_error"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []domain.EpisodeEntry
			for _, message := range tc.errors {
				entries = append(entries, domain.EpisodeEntry{SessionKey: "room", ModelKind: tc.kind, Error: message})
			}
			got := mine(entries, nil)
			require.Len(t, got, 1)
			require.Equal(t, tc.want, got[0].Errors)
			require.Equal(t, tc.wantSignals, got[0].Signals)
		})
	}
}

func TestQueuedInputDoesNotBorrowThePreviousTurnsReply(t *testing.T) {
	entries := []domain.EpisodeEntry{
		{ID: "a", SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "first"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnStarted},
		{SessionKey: "room", ModelKind: domain.ModelEventToolResult, Error: "first error"},
		{ID: "b", SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "queued second"},
		{SessionKey: "room", ModelKind: domain.ModelEventError, Error: "second error"},
		{SessionKey: "room", ModelKind: domain.ModelEventToolCall, ToolCall: &domain.ToolCall{Name: "reply", Input: json.RawMessage(`{"text":"answer to first"}`)}},
		{SessionKey: "room", Sent: true, Text: "answer to first"},
	}
	got := mine(entries, nil)
	require.Len(t, got, 2)
	require.Equal(t, []string{"first error"}, got[0].Errors)
	require.Equal(t, []string{"second error"}, got[1].Errors)
	for _, episode := range got {
		require.True(t, episode.Ambiguous)
		require.Contains(t, episode.Signals, "tool_error")
		require.Empty(t, episode.Tools)
		require.Empty(t, episode.Reply)
	}
}

func TestOverlapRemainsAmbiguousAcrossQueuedTurns(t *testing.T) {
	entries := []domain.EpisodeEntry{
		{SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "A"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnStarted},
		{SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "B"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnFinished},
		{SessionKey: "room", InboundKind: domain.InboundEventMessage, Text: "C"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnStarted},
		{SessionKey: "room", Sent: true, Text: "answer B"},
	}
	got := mine(entries, nil)
	require.Len(t, got, 3)
	for _, episode := range got {
		require.True(t, episode.Ambiguous)
		require.Empty(t, episode.Reply)
	}
}

func TestIgnoredChatDoesNotExcludeAcceptedTurns(t *testing.T) {
	yes, no := true, false
	entries := []domain.EpisodeEntry{
		{SessionKey: "room", InboundKind: domain.InboundEventMessage, Accepted: &no, Text: "ambient"},
		{SessionKey: "room", InboundKind: domain.InboundEventMessage, Accepted: &yes, Text: "accepted mention"},
		{SessionKey: "room", ModelKind: domain.ModelEventTurnStarted},
		{SessionKey: "room", Sent: true, Text: "answer"},
	}
	got := mine(entries, nil)
	require.Len(t, got, 2)
	require.True(t, got[0].ContextOnly)
	require.False(t, got[1].Ambiguous)
	require.Equal(t, "answer", got[1].Reply)
}
