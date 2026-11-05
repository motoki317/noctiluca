package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type registration struct {
	Resource string        `json:"resource"`
	OAuth    oauth2.Config `json:"oauth"`
}

type tokenSource struct {
	mu           sync.Mutex
	ctx          context.Context
	repository   domain.OAuthTokenRepository
	server       string
	registration json.RawMessage
	source       oauth2.TokenSource
	saved        *oauth2.Token
}

func (s *tokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.source.Token()
	if err != nil {
		// OAuth error bodies can contain credentials and must not reach logs or model inputs.
		return nil, errors.New("MCP token refresh failed; run mcp-login again")
	}
	if s.saved == nil || token.AccessToken != s.saved.AccessToken || token.RefreshToken != s.saved.RefreshToken || !token.Expiry.Equal(s.saved.Expiry) || token.TokenType != s.saved.TokenType {
		err := s.repository.Save(s.ctx, s.server, domain.OAuthToken{Registration: s.registration, AccessToken: token.AccessToken, TokenType: token.TokenType, RefreshToken: token.RefreshToken, Expiry: token.Expiry})
		if err != nil {
			return nil, errors.New("could not persist MCP tokens")
		}
		copy := *token
		s.saved = &copy
	}
	return token, nil
}

func restoreTokens(ctx context.Context, config ServerConfig, repository domain.OAuthTokenRepository) (oauth2.TokenSource, error) {
	stored, err := repository.Get(ctx, config.Name)
	if err != nil {
		return nil, err
	}
	var reg registration
	if json.Unmarshal(stored.Registration, &reg) != nil || reg.Resource != config.URL || reg.OAuth.ClientID == "" || !validURL(reg.OAuth.Endpoint.TokenURL) || stored.AccessToken == "" {
		return nil, domain.ErrNotFound
	}
	token := &oauth2.Token{AccessToken: stored.AccessToken, TokenType: stored.TokenType, RefreshToken: stored.RefreshToken, Expiry: stored.Expiry}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient())
	return &tokenSource{ctx: ctx, repository: repository, server: config.Name, registration: stored.Registration, source: reg.OAuth.TokenSource(ctx, token), saved: token}, nil
}

func oauthHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
