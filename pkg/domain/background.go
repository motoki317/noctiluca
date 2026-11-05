package domain

import "context"

type Background interface {
	Shutdown(context.Context) error
	Start(context.Context) error
	Tools() []Tool
}
