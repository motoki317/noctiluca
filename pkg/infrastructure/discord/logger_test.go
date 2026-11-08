package discord

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func TestDisgoLogging(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.Debug("application debug")
	require.Contains(t, logs.String(), "application debug")
	logs.Reset()

	a := testAdapter(t, Config{}, nil)
	_, err := a.DirectMessage(t.Context(), "200")
	require.NoError(t, err)
	assert.Empty(t, logs.String(), "REST request and response bodies must stay out of DEBUG logs")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created := false
	a.createGateway = func(_ string, _ gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
		created = true
		logger := applyGatewayOptions(opts).Logger
		require.NotNil(t, logger)
		logger = logger.With("name", "gateway").WithGroup("wire")
		logger.Debug("sending gateway message", "data", "fake-token")
		assert.Empty(t, logs.String())
		for _, level := range []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
			logger.Log(ctx, level, "gateway diagnostic", "operation", "connect")
		}
		cancel()
		return &stubGateway{}
	}
	require.NoError(t, a.Start(ctx, func(context.Context, domain.InboundEvent) error { t.Fatal("unexpected event"); return nil }))
	require.True(t, created)
	assert.Equal(t, 3, strings.Count(logs.String(), "gateway diagnostic"))
	assert.Contains(t, logs.String(), `"name":"gateway"`)
	assert.Contains(t, logs.String(), `"wire":{"operation":"connect"}`)
	assert.NotContains(t, logs.String(), "fake-token")
}
