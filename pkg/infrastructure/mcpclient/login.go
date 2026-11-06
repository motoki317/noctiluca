package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

// Login prints each authorization URL to output and, when open is non-nil, also opens it in a
// browser. A failed open is ignored: the printed URL still works.
func Login(ctx context.Context, config ServerConfig, repository domain.OAuthTokenRepository, output io.Writer, open func(string) error) error {
	if err := Validate([]ServerConfig{config}); err != nil {
		return err
	}
	httpClient := oauthHTTPClient(config.URL)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for OAuth redirect: %w", err)
	}
	// One callback server serves the whole login: the SDK can ask for authorization more than
	// once (for example after a scope challenge), and a per-request server would close the listener.
	callbacks := serveCallbacks(listener)
	defer callbacks.close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	var reg *registration
	var audience string
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		// Slite returns a matching iss without advertising RFC 9207 support. The SDK still rejects a mismatch.
		AcceptUnadvertisedIss: true,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "noctiluca", RedirectURIs: []string{redirect},
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
		}},
		ScopeFilter: func(discovered []string) []string {
			if config.Scopes != nil {
				return config.Scopes
			}
			return discovered
		},
		Client: httpClient,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			u, err := url.Parse(args.URL)
			if err != nil || !validURL(args.URL) {
				return nil, errors.New("invalid OAuth authorization URL")
			}
			audience = u.Query().Get("resource")
			return callbacks.receive(ctx, args.URL, output, open)
		},
		NewTokenSource: func(refreshCtx context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			reg = &registration{Resource: config.URL, Audience: audience, LoginID: uuid.NewString(), OAuth: *cfg}
			reg.OAuth.Scopes = grantedScopes(token, cfg.Scopes)
			return refreshSource(refreshCtx, cfg, token, audience), nil
		},
	})
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "noctiluca", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: config.URL, OAuthHandler: handler, HTTPClient: oauthHTTPClient(config.URL)}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The cause names the failed OAuth step (registration, consent, token exchange).
		return fmt.Errorf("MCP login failed: %w", err)
	}
	defer session.Close()
	source, _ := handler.TokenSource(ctx)
	if source == nil || reg == nil {
		return errors.New("MCP server did not request OAuth authorization")
	}
	token, err := source.Token()
	if err != nil {
		return errors.New("MCP login token is unavailable")
	}
	// Keep the old login until the replacement completes the MCP handshake.
	return saveToken(ctx, repository, config.Name, *reg, token)
}

type callbackResult struct {
	result *auth.AuthorizationResult
	err    error
}

type callbackServer struct {
	server  *http.Server
	serve   chan error
	mu      sync.Mutex
	state   string              // state of the authorization request being waited for
	results chan callbackResult // capacity 1: the browser can return before receive starts waiting
}

func serveCallbacks(listener net.Listener) *callbackServer {
	c := &callbackServer{serve: make(chan error, 1), results: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		c.mu.Lock()
		valid := c.state != "" && q.Get("state") == c.state
		if valid {
			c.state = ""
		}
		c.mu.Unlock()
		if !valid {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		var result callbackResult
		if q.Get("error") != "" || q.Get("code") == "" {
			// The SDK only validates iss on successful callbacks. Never display an unvalidated error response.
			result.err = errors.New("authorization denied or code missing")
		} else {
			result.result = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
		}
		c.results <- result
		fmt.Fprintln(w, "Authorization response received. Check the terminal for the login result.")
	})
	c.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { c.serve <- c.server.Serve(listener) }()
	return c
}

// receive prints the authorization URL and waits for the browser's redirect for that request.
func (c *callbackServer) receive(ctx context.Context, authorizationURL string, output io.Writer, open func(string) error) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.state = u.Query().Get("state")
	c.mu.Unlock()
	if _, err := fmt.Fprintln(output, authorizationURL); err != nil {
		return nil, err
	}
	if open != nil {
		_ = open(authorizationURL)
	}
	select {
	case r := <-c.results:
		return r.result, r.err
	case err := <-c.serve:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *callbackServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c.server.Shutdown(ctx) != nil {
		_ = c.server.Close()
	}
}
