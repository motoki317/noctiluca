package discord

import (
	"context"
	"log/slog"
)

func disgoLogger() *slog.Logger {
	// Disgo's DEBUG wire dumps include bot tokens and message text.
	return slog.New(infoHandler{slog.Default().Handler()})
}

type infoHandler struct{ slog.Handler }

func (h infoHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && h.Handler.Enabled(ctx, level)
}

func (h infoHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return infoHandler{h.Handler.WithAttrs(attrs)}
}

func (h infoHandler) WithGroup(name string) slog.Handler {
	return infoHandler{h.Handler.WithGroup(name)}
}
