package memory

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed heartbeat.md
var heartbeatFrame string

// Original text: MIT (LICENSE). Research ideas: ACE, Reflexion, Letta, and Mem0.
// Sources retain their own licenses. See THIRD_PARTY_NOTICES.md.
//
//go:embed rest.md
var restFrame string

func Procedure(_ context.Context, name string) (string, error) {
	switch name {
	case "heartbeat":
		return heartbeatFrame, nil
	case "rest":
		return restFrame, nil
	default:
		return "", fmt.Errorf("unknown procedure %q", name)
	}
}
