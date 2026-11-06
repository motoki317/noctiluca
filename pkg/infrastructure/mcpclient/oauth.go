package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type registration struct {
	Resource string        `json:"resource"`
	Audience string        `json:"audience,omitempty"`
	LoginID  string        `json:"login_id,omitempty"`
	OAuth    oauth2.Config `json:"oauth"`
}

type tokenSource struct {
	mu           sync.Mutex
	ctx          context.Context
	repository   domain.OAuthTokenRepository
	server       string
	registration registration
	source       oauth2.TokenSource
	saved        *oauth2.Token
	stored       domain.OAuthToken
	failed       chan struct{}
	failure      error
}

func (s *tokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenLocked()
}

func (s *tokenSource) tokenLocked() (*oauth2.Token, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	if s.saved.RefreshToken == "" && !s.saved.Valid() {
		return nil, s.failLocked("token expired without a refresh token")
	}
	token, err := s.source.Token()
	if err != nil {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		// OAuth error bodies can contain credentials and must not reach logs or model inputs.
		var rejected *oauth2.RetrieveError
		if errors.As(err, &rejected) && rejected.Response != nil && rejected.Response.StatusCode < 500 && rejected.Response.StatusCode != http.StatusTooManyRequests && rejected.Response.StatusCode != http.StatusRequestTimeout && rejected.ErrorCode != "temporarily_unavailable" && rejected.ErrorCode != "server_error" {
			return nil, s.failLocked("token refresh rejected")
		}
		return nil, errors.New("MCP token refresh failed; retry later")
	}
	scopes := grantedScopes(token, s.registration.OAuth.Scopes)
	if s.saved == nil || token.AccessToken != s.saved.AccessToken || token.RefreshToken != s.saved.RefreshToken || !token.Expiry.Equal(s.saved.Expiry) || token.TokenType != s.saved.TokenType || !slices.Equal(scopes, s.registration.OAuth.Scopes) {
		reg := s.registration
		reg.OAuth.Scopes = scopes
		stored, err := encodeToken(reg, token)
		if err != nil {
			return nil, err
		}
		saved, err := s.repository.Refresh(s.ctx, s.server, s.stored, stored)
		if err != nil {
			return nil, errors.New("could not persist MCP tokens")
		}
		if !saved {
			return nil, s.failLocked("stored login changed; restart noctiluca to load it")
		}
		copy := *token
		s.saved = &copy
		s.registration = reg
		s.stored = stored
	}
	return token, nil
}

func (s *tokenSource) refreshRejected(bearer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.tokenLocked(); err != nil {
		return err
	}
	// Concurrent requests can reject a token that another request already refreshed.
	if bearer != "Bearer "+s.saved.AccessToken {
		return nil
	}
	if s.saved.RefreshToken == "" {
		return s.failLocked("authorization rejected (401)")
	}
	expired := *s.saved
	expired.Expiry = time.Now().Add(-time.Minute)
	s.source = refreshSource(s.ctx, &s.registration.OAuth, &expired, s.registration.Audience)
	_, err := s.tokenLocked()
	return err
}

func (s *tokenSource) failLocked(reason string) error {
	if s.failure == nil {
		s.failure = fmt.Errorf("MCP server %s: %s; run mcp-login %s, then restart noctiluca", s.server, reason, s.server)
		close(s.failed)
	}
	return s.failure
}

func (s *tokenSource) fail(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failLocked(reason)
}

func (s *tokenSource) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *tokenSource) expiresWithoutRefresh() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved.RefreshToken == "" {
		return s.saved.Expiry
	}
	return time.Time{}
}

func restoreTokens(ctx context.Context, config ServerConfig, repository domain.OAuthTokenRepository) (*tokenSource, error) {
	stored, err := repository.Get(ctx, config.Name)
	if err != nil {
		return nil, err
	}
	var reg registration
	if json.Unmarshal(stored.Registration, &reg) != nil || reg.Resource != config.URL || reg.OAuth.ClientID == "" || !validURL(reg.OAuth.Endpoint.TokenURL) || stored.AccessToken == "" {
		return nil, domain.ErrNotFound
	}
	token := &oauth2.Token{AccessToken: stored.AccessToken, TokenType: stored.TokenType, RefreshToken: stored.RefreshToken, Expiry: stored.Expiry}
	if reg.Audience == "" {
		reg.Audience = reg.Resource
	}
	if reg.LoginID == "" {
		// Older logins need a stable notice key across restarts without exposing credentials.
		reg.LoginID = fmt.Sprintf("%x", sha256.Sum256(stored.Registration))
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient(config.URL))
	return &tokenSource{ctx: ctx, repository: repository, server: config.Name, registration: reg, source: refreshSource(ctx, &reg.OAuth, token, reg.Audience), saved: token, stored: stored, failed: make(chan struct{})}, nil
}

func grantedScopes(token *oauth2.Token, fallback []string) []string {
	if scope, ok := token.Extra("scope").(string); ok {
		return strings.Fields(scope)
	}
	return fallback
}

func saveToken(ctx context.Context, repository domain.OAuthTokenRepository, server string, reg registration, token *oauth2.Token) error {
	stored, err := encodeToken(reg, token)
	if err != nil {
		return err
	}
	return repository.Save(ctx, server, stored)
}

func encodeToken(reg registration, token *oauth2.Token) (domain.OAuthToken, error) {
	reg.OAuth.Scopes = grantedScopes(token, reg.OAuth.Scopes)
	raw, err := json.Marshal(reg)
	if err != nil {
		return domain.OAuthToken{}, err
	}
	return domain.OAuthToken{Registration: raw, AccessToken: token.AccessToken, TokenType: token.TokenType, RefreshToken: token.RefreshToken, Expiry: token.Expiry}, nil
}

func refreshSource(ctx context.Context, config *oauth2.Config, token *oauth2.Token, resource string) oauth2.TokenSource {
	client := *ctx.Value(oauth2.HTTPClient).(*http.Client)
	base := client.Transport
	// oauth2 owns refresh and rotation, but its Config has no refresh-parameter hook.
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			body, err := io.ReadAll(request.Body)
			_ = request.Body.Close()
			if err != nil {
				return nil, err
			}
			form, err := url.ParseQuery(string(body))
			if err != nil {
				return nil, err
			}
			form.Set("resource", resource)
			request = request.Clone(request.Context())
			encoded := form.Encode()
			request.Body = io.NopCloser(strings.NewReader(encoded))
			request.ContentLength = int64(len(encoded))
		}
		return base.RoundTrip(request)
	})
	return config.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, &client), token)
}
