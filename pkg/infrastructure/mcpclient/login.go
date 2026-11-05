package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func Login(ctx context.Context, config ServerConfig, repository domain.OAuthTokenRepository, output io.Writer) error {
	if err := Validate([]ServerConfig{config}); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for OAuth redirect: %w", err)
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL:                     redirect,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "noctiluca", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", Scope: strings.Join(config.Scopes, " ")}},
		ScopeFilter: func(discovered []string) []string {
			if config.Scopes != nil {
				return append([]string(nil), config.Scopes...)
			}
			return discovered
		},
		// Explicit scopes must match registration. Include offline_access in config if the server requires it.
		Client: oauthHTTPClient(),
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return receiveCode(ctx, listener, args.URL, output)
		},
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if token.RefreshToken == "" {
				return nil, errors.New("authorization server did not issue a refresh token")
			}
			reg, err := json.Marshal(registration{Resource: config.URL, OAuth: *cfg})
			if err != nil {
				return nil, err
			}
			refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient())
			source := &tokenSource{ctx: refreshCtx, repository: repository, server: config.Name, registration: reg, source: cfg.TokenSource(refreshCtx, token)}
			_, err = source.Token()
			return source, err
		},
	})
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "noctiluca", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: config.URL, OAuthHandler: handler, HTTPClient: oauthHTTPClient()}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("MCP login failed: check server OAuth support, consent, and configured scopes")
	}
	defer session.Close()
	source, _ := handler.TokenSource(ctx)
	if source == nil {
		return errors.New("MCP server did not request OAuth authorization")
	}
	return nil
}

func receiveCode(ctx context.Context, listener net.Listener, authorizationURL string, output io.Writer) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(authorizationURL)
	if err != nil {
		return nil, err
	}
	state := u.Query().Get("state")
	type callback struct {
		result *auth.AuthorizationResult
		err    error
	}
	results := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if state == "" || q.Get("state") != state {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		var result callback
		if q.Get("error") != "" || q.Get("code") == "" {
			result.err = errors.New("authorization denied")
		} else {
			result.result = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
		}
		select {
		case results <- result:
			fmt.Fprintln(w, "Authorization response received. Check the terminal for the login result.")
		default:
			http.Error(w, "Authorization response already received", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if server.Shutdown(shutdownCtx) != nil {
			_ = server.Close()
		}
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	if _, err := fmt.Fprintln(output, authorizationURL); err != nil {
		return nil, err
	}
	select {
	case result := <-results:
		return result.result, result.err
	case err := <-serveErr:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
