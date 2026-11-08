package discord

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type unusedClient struct{ rest.Client }

func (*unusedClient) Close(context.Context) {}

func retryAdapter(t *testing.T) *adapter {
	t.Helper()
	a := &adapter{client: &unusedClient{}}
	t.Cleanup(func() { require.NoError(t, a.Shutdown(context.Background())) })
	return a
}

// Type inference exposes the SDK's option values without opening its gateway.
func applyGatewayOptions[C any, O ~func(*C)](opts []O) *C {
	var config C
	for _, opt := range opts {
		opt(&config)
	}
	return &config
}

func TestGatewayRetryDelay(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "open failure", true: "terminal close"}[terminal], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := retryAdapter(t)
				var openedAt time.Time
				a.createGateway = func(_ string, _ gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
					openedAt = time.Now()
					return &stubGateway{onOpen: func(context.Context) error {
						if terminal {
							applyGatewayOptions(opts).CloseHandler(nil, errors.New("disallowed intent"), false)
							return nil
						}
						return errors.New("open failed")
					}}
				}
				for _, delay := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute} {
					before := time.Now()
					require.Error(t, a.Start(t.Context(), func(context.Context, domain.InboundEvent) error { t.Fatal("unexpected consume"); return nil }))
					assert.Equal(t, delay, openedAt.Sub(before))
				}
			})
		})
	}
}

func TestGatewayRetryResetAfterStableSession(t *testing.T) {
	for _, duration := range []time.Duration{10*time.Minute - time.Nanosecond, 10 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := retryAdapter(t)
				attempt := 0
				var openedAt time.Time
				a.createGateway = func(_ string, _ gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
					attempt++
					openedAt = time.Now()
					return &stubGateway{onOpen: func(context.Context) error {
						if attempt != 4 {
							return errors.New("open failed")
						}
						go func() {
							time.Sleep(duration)
							synctest.Wait()
							applyGatewayOptions(opts).CloseHandler(nil, errors.New("closed"), false)
						}()
						return nil
					}}
				}
				consume := func(context.Context, domain.InboundEvent) error { t.Fatal("unexpected consume"); return nil }
				for range 4 {
					require.Error(t, a.Start(t.Context(), consume))
				}
				before := time.Now()
				require.Error(t, a.Start(t.Context(), consume))
				want := 8 * time.Second
				if duration == 10*time.Minute {
					want = time.Second
				}
				assert.Equal(t, want, openedAt.Sub(before))
			})
		})
	}
}

func TestGatewayRetryWaitCancels(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "shutdown"}[shutdown], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := retryAdapter(t)
				opens := 0
				a.createGateway = func(_ string, _ gateway.EventHandlerFunc, _ ...gateway.ConfigOpt) gateway.Gateway {
					opens++
					return &stubGateway{openErr: errors.New("open failed")}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				consume := func(context.Context, domain.InboundEvent) error { t.Fatal("unexpected consume"); return nil }
				require.Error(t, a.Start(ctx, consume))
				done := make(chan error, 1)
				go func() { done <- a.Start(ctx, consume) }()
				synctest.Wait()
				if shutdown {
					require.NoError(t, a.Shutdown(t.Context()))
				} else {
					cancel()
				}
				require.NoError(t, <-done)
				assert.Equal(t, 1, opens)
			})
		})
	}
}

type retryHistory struct{ domain.EventRepository }

func (*retryHistory) GetInbound(context.Context, domain.ConversationRef) (domain.InboundEvent, error) {
	return domain.InboundEvent{Conversation: "chat:400:", MessageRef: "chat:400:600"}, nil
}

type retryREST struct{ rest.Rest }

func (*retryREST) CreateDMChannel(snowflake.ID, ...rest.RequestOpt) (*sdk.DMChannel, error) {
	return nil, errors.New("owner unavailable")
}

func TestGatewayRetryRequiresContinuousConnection(t *testing.T) {
	for _, scenario := range []string{"close during consume", "short resumed session", "stable resumed session"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := retryAdapter(t)
				a.history = &retryHistory{}
				a.api = &retryREST{}
				var status atomic.Int32
				status.Store(int32(gateway.StatusReady))
				var listener gateway.EventHandlerFunc
				var closeHandler gateway.CloseHandlerFunc
				var openedAt time.Time
				attempt := 0
				opened := make(chan struct{})
				a.createGateway = func(_ string, handle gateway.EventHandlerFunc, opts ...gateway.ConfigOpt) gateway.Gateway {
					attempt++
					openedAt = time.Now()
					if attempt != 4 {
						return &stubGateway{openErr: errors.New("open failed")}
					}
					listener, closeHandler = handle, applyGatewayOptions(opts).CloseHandler
					return &stubGateway{status: func() gateway.Status { return gateway.Status(status.Load()) }, onOpen: func(context.Context) error {
						if scenario == "close during consume" {
							listener(nil, "", 1, gateway.EventMessageDelete{ID: 600, ChannelID: 400})
						}
						close(opened)
						return nil
					}}
				}
				release := make(chan struct{})
				stop := errors.New("consume failed")
				consume := func(context.Context, domain.InboundEvent) error { <-release; return stop }
				for range 3 {
					require.Error(t, a.Start(t.Context(), consume))
				}
				done := make(chan error, 1)
				go func() { done <- a.Start(t.Context(), consume) }()
				<-opened
				synctest.Wait()
				time.Sleep(9 * time.Minute)
				status.Store(int32(gateway.StatusDisconnected))
				if scenario == "close during consume" {
					closeHandler(nil, errors.New("closed"), false)
				}
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				if scenario == "close during consume" {
					close(release)
				} else {
					status.Store(int32(gateway.StatusReady))
					listener(nil, "", 2, gateway.EventResumed{})
					duration := 10 * time.Minute
					if scenario == "short resumed session" {
						duration -= time.Nanosecond
					}
					time.Sleep(duration)
					synctest.Wait()
					status.Store(int32(gateway.StatusDisconnected))
					closeHandler(nil, errors.New("closed"), false)
				}
				require.Error(t, <-done)
				before := time.Now()
				require.Error(t, a.Start(t.Context(), consume))
				want := 8 * time.Second
				if scenario == "stable resumed session" {
					want = time.Second
				}
				assert.Equal(t, want, openedAt.Sub(before))
			})
		})
	}
}
