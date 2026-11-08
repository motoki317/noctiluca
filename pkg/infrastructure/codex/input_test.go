package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func inputTestServer(mode string, emit func(any), notify func(string, any)) func(rpcFrame) bool {
	count := 0
	active := ""
	var held *rpcFrame
	return func(f rpcFrame) bool {
		if !strings.HasPrefix(mode, "input-") {
			return false
		}
		response := rpcFrame{ID: f.ID, Result: json.RawMessage(`{}`)}
		switch f.Method {
		case "turn/start", "turn/steer":
			var params struct {
				ExpectedTurnID string                  `json:"expectedTurnId"`
				Input          []struct{ Text string } `json:"input"`
			}
			_ = json.Unmarshal(f.Params, &params)
			if f.Method == "turn/start" {
				count++
				active = fmt.Sprintf("turn-%d", count)
				response.Result, _ = json.Marshal(map[string]any{"turn": map[string]string{"id": active}})
				notify("turn/started", map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": active}})
			} else {
				response.Result, _ = json.Marshal(map[string]string{"turnId": active})
				if strings.Contains(mode, "refuse") && count == 1 {
					response.Error = &rpcError{Code: -32600, Message: "cannot steer a compact turn"}
					response.Result = nil
				}
				if mode == "input-invalid" {
					response.Result = json.RawMessage(`{"turnId":"wrong-turn"}`)
				}
				if params.ExpectedTurnID != active {
					response.Error = &rpcError{Code: -32600, Message: "expectedTurnId mismatch"}
					response.Result = nil
				}
			}
			if (mode == "input-start-late" && count == 1 && f.Method == "turn/start") ||
				(strings.Contains(mode, "steer-late") && f.Method == "turn/steer") {
				held = &response
			} else {
				emit(response)
			}
			notify("item/agentMessage/delta", map[string]string{"threadId": "thread-1", "delta": f.Method + ":" + params.Input[0].Text})
			return true
		case "test/release":
			if held != nil {
				emit(*held)
				held = nil
			}
		case "test/complete":
			notify("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": active, "status": "completed"}})
			active = ""
		case "test/notify":
			var note rpcFrame
			_ = json.Unmarshal(f.Params, &note)
			emit(note)
		default:
			return false
		}
		emit(response)
		return true
	}
}

func openInputSession(t *testing.T, mode string) (*session, string) {
	t.Helper()
	path, dir := fakeServer(t, mode)
	model, err := New(path, "", "").Open(t.Context(), domain.ModelSessionConfig{Workdir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, model.Close()) })
	return model.(*session), dir
}

func control(t *testing.T, s *session, method string, params any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.call(ctx, "test/"+method, params, nil))
}

func requireEvents(t *testing.T, s domain.ModelSession, expected ...domain.ModelEvent) {
	t.Helper()
	var events []domain.ModelEvent
	for range expected {
		events = append(events, nextEvent(t, s))
	}
	require.ElementsMatch(t, expected, events)
}

func sendAndStart(t *testing.T, s domain.ModelSession, text string) {
	t.Helper()
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: text}))
	requireEvents(t, s,
		domain.ModelEvent{Kind: domain.ModelEventTurnStarted},
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 1},
		domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/start:" + text},
	)
}

func TestSteerTracksInputsThroughCompletion(t *testing.T) {
	s, dir := openInputSession(t, "input-normal")
	sendAndStart(t, s, "first")
	for i, text := range []string{"", "second", "second"} {
		require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: int64(i + 2), Text: text}))
		requireEvents(t, s,
			domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: int64(i + 2)},
			domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/steer:" + text},
		)
		require.False(t, s.Idle(), "steering acceptance does not finish the turn")
	}
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
	var methods []string
	for _, f := range requests(t, dir) {
		if f.Method == "turn/start" || f.Method == "turn/steer" {
			methods = append(methods, f.Method)
			var params map[string]any
			require.NoError(t, json.Unmarshal(f.Params, &params))
			require.Equal(t, "thread-1", params["threadId"])
			if f.Method == "turn/steer" {
				require.Equal(t, "turn-1", params["expectedTurnId"])
			}
		}
	}
	require.Equal(t, []string{"turn/start", "turn/steer", "turn/steer", "turn/steer"}, methods)
}

func TestRefusedSteeringQueuesInOrder(t *testing.T) {
	s, dir := openInputSession(t, "input-refuse")
	sendAndStart(t, s, "first")
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "queued"}))
	require.Equal(t, "turn/steer:queued", nextEvent(t, s).Text)
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 3, Text: "last"}))
	require.False(t, s.Idle())
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.False(t, s.Idle(), "completion of the refusing turn does not cover queued input")
	requireEvents(t, s,
		domain.ModelEvent{Kind: domain.ModelEventTurnStarted},
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 2},
		domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/start:queued"},
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 3},
		domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/steer:last"},
	)
	control(t, s, "notify", rpcFrame{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)})
	require.Empty(t, s.events, "an old completion must not finish the core's new turn")
	require.False(t, s.Idle(), "an old completion must not clear the new turn")
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
	var inputs []string
	for _, f := range requests(t, dir) {
		if f.Method == "turn/start" || f.Method == "turn/steer" {
			var params struct{ Input []struct{ Text string } }
			require.NoError(t, json.Unmarshal(f.Params, &params))
			inputs = append(inputs, f.Method+":"+params.Input[0].Text)
		}
	}
	require.Equal(t, []string{"turn/start:first", "turn/steer:queued", "turn/start:queued", "turn/steer:last"}, inputs)
}

func TestCompletionBeforeInputResponse(t *testing.T) {
	for _, mode := range []string{"input-start-late", "input-steer-late", "input-refuse-steer-late"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := openInputSession(t, mode)
			if mode == "input-start-late" {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "first"}))
				require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
				require.Equal(t, "turn/start:first", nextEvent(t, s).Text)
			} else {
				sendAndStart(t, s, "first")
			}
			if mode != "input-start-late" {
				require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "second"}))
				require.Equal(t, "turn/steer:second", nextEvent(t, s).Text)
			}
			control(t, s, "complete", nil)
			require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
			if mode != "input-start-late" {
				require.False(t, s.Idle(), "steering can still be refused until its reply arrives")
			}
			control(t, s, "release", nil)
			if mode == "input-refuse-steer-late" {
				requireEvents(t, s,
					domain.ModelEvent{Kind: domain.ModelEventTurnStarted},
					domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 2},
					domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/start:second"},
				)
				require.False(t, s.Idle())
				control(t, s, "complete", nil)
				require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
			} else {
				id := int64(2)
				if mode == "input-start-late" {
					id = 1
				}
				require.Equal(t, domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: id}, nextEvent(t, s))
			}
			require.Empty(t, s.events)
			require.Eventually(t, s.Idle, time.Second, time.Millisecond, "late success must not revive a completed turn")
		})
	}
}

func TestInputWhileStartResponseIsPending(t *testing.T) {
	s, _ := openInputSession(t, "input-start-late")
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "first"}))
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, "turn/start:first", nextEvent(t, s).Text)
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "second"}))
	require.False(t, s.Idle())
	control(t, s, "release", nil)
	requireEvents(t, s,
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 1},
		domain.ModelEvent{Kind: domain.ModelEventInputStarted, InputID: 2},
		domain.ModelEvent{Kind: domain.ModelEventText, Text: "turn/steer:second"},
	)
	control(t, s, "complete", nil)
	require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
	require.Eventually(t, s.Idle, time.Second, time.Millisecond)
}

func TestCanceledInputIsNotQueued(t *testing.T) {
	s, dir := openInputSession(t, "input-normal")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.Send(ctx, domain.UserInput{Text: "canceled"}), context.Canceled)
	require.True(t, s.Idle())
	require.NoError(t, s.Close())
	require.Len(t, requests(t, dir), 3)
}

func TestCloseWithPendingInputResponse(t *testing.T) {
	s, dir := openInputSession(t, "input-start-late")
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 1, Text: "first"}))
	require.Equal(t, domain.ModelEventTurnStarted, nextEvent(t, s).Kind)
	require.Equal(t, "turn/start:first", nextEvent(t, s).Text)
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "queued"}))
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on the missing turn/start response")
	}
	frames := requests(t, dir)
	require.Len(t, frames, 5)
	require.Equal(t, "turn/interrupt", frames[4].Method)
}

func TestInvalidSteerResponseClosesSession(t *testing.T) {
	s, _ := openInputSession(t, "input-invalid")
	sendAndStart(t, s, "first")
	require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: 2, Text: "second"}))
	for {
		event := nextEvent(t, s)
		if event.Kind == domain.ModelEventError {
			require.ErrorContains(t, event.Err, "invalid codex response to turn/steer")
			break
		}
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session survived a mismatched turn ID")
	}
}

func TestTurnStartRefused(t *testing.T) {
	s, _ := openInputSession(t, "reject")
	for _, id := range []int64{41, 42} {
		require.NoError(t, s.Send(t.Context(), domain.UserInput{ID: id, Text: "rejected"}))
		event := nextEvent(t, s)
		require.Equal(t, domain.ModelEventInputRefused, event.Kind)
		require.Equal(t, id, event.InputID)
		require.ErrorContains(t, event.Err, "turn rejected")
		require.Equal(t, domain.ModelEventTurnFinished, nextEvent(t, s).Kind)
		require.True(t, s.Idle())
		require.Empty(t, s.events)
	}
}
