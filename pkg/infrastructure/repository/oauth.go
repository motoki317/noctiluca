package repository

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type oauthTokenRepository struct{ db *sql.DB }

func NewOAuthTokenRepository(db *sql.DB) domain.OAuthTokenRepository {
	return &oauthTokenRepository{db: db}
}

func (r *oauthTokenRepository) Get(ctx context.Context, server string) (domain.OAuthToken, error) {
	var token domain.OAuthToken
	var registration string
	err := r.db.QueryRowContext(ctx, `SELECT access_token, token_type, refresh_token, expiry, registration_json FROM oauth_tokens WHERE server=?`, server).
		Scan(&token.AccessToken, &token.TokenType, &token.RefreshToken, &token.Expiry, &registration)
	if registration != "null" && registration != "" {
		token.Registration = json.RawMessage(registration)
	}
	return token, mapNotFound(err)
}

func (r *oauthTokenRepository) Save(ctx context.Context, server string, token domain.OAuthToken) error {
	registration := token.Registration
	if len(registration) == 0 {
		registration = json.RawMessage(`null`)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO oauth_tokens(server, access_token, token_type, refresh_token, expiry, registration_json) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(server) DO UPDATE SET access_token=excluded.access_token, token_type=excluded.token_type,
		refresh_token=excluded.refresh_token, expiry=excluded.expiry, registration_json=excluded.registration_json`, server, token.AccessToken, token.TokenType, token.RefreshToken, token.Expiry.UTC(), string(registration))
	return err
}

// Refresh cannot overwrite a newer login or another process's token rotation.
func (r *oauthTokenRepository) Refresh(ctx context.Context, server string, previous, token domain.OAuthToken) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE oauth_tokens SET access_token=?, token_type=?, refresh_token=?, expiry=?, registration_json=?
		WHERE server=? AND access_token=? AND refresh_token=? AND registration_json=?`, token.AccessToken, token.TokenType, token.RefreshToken, token.Expiry.UTC(), string(token.Registration), server, previous.AccessToken, previous.RefreshToken, string(previous.Registration))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
