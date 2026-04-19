package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/oauth2"
)

// TokenStore is the persistence layer for OAuth tokens.
// Implemented by the KV store adapter in localhooks/kvtokenstore.go.
type TokenStore interface {
	GetOAuthToken(ctx context.Context, serverName string) (*oauth2.Token, error)
	SetOAuthToken(ctx context.Context, serverName string, t *oauth2.Token) error
	DeleteOAuthToken(ctx context.Context, serverName string) error

	GetClientRegistration(ctx context.Context, serverName string) (*ClientRegistration, error)
	SetClientRegistration(ctx context.Context, serverName string, reg *ClientRegistration) error
}

// storedToken is the on-disk representation. We use RFC3339 for Expiry so
// that it survives JSON round-trips without timezone loss.
type storedToken struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Expiry       string `json:"expiry,omitempty"` // RFC3339 or empty
}

// KVStore is the minimal interface needed from runtimetypes.Store.
// Using a narrow interface avoids importing runtimetypes in this package.
type KVStore interface {
	GetKV(ctx context.Context, key string, out interface{}) error
	SetKV(ctx context.Context, key string, value json.RawMessage) error
	DeleteKV(ctx context.Context, key string) error
}

// NewKVTokenStore returns a TokenStore backed by the provided KVStore.
func NewKVTokenStore(kv KVStore) TokenStore {
	return &kvTokenStore{kv: kv}
}

type kvTokenStore struct {
	kv KVStore
}

func (s *kvTokenStore) GetOAuthToken(ctx context.Context, name string) (*oauth2.Token, error) {
	var st storedToken
	if err := s.kv.GetKV(ctx, tokenKey(name), &st); err != nil {
		return nil, err
	}
	t := &oauth2.Token{
		AccessToken:  st.AccessToken,
		TokenType:    st.TokenType,
		RefreshToken: st.RefreshToken,
	}
	if st.Expiry != "" {
		exp, err := time.Parse(time.RFC3339, st.Expiry)
		if err == nil {
			t.Expiry = exp
		}
	}
	return t, nil
}

func (s *kvTokenStore) SetOAuthToken(ctx context.Context, name string, t *oauth2.Token) error {
	st := storedToken{
		AccessToken:  t.AccessToken,
		TokenType:    t.TokenType,
		RefreshToken: t.RefreshToken,
	}
	if !t.Expiry.IsZero() {
		st.Expiry = t.Expiry.UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("mcpoauth: marshal token: %w", err)
	}
	return s.kv.SetKV(ctx, tokenKey(name), json.RawMessage(b))
}

func (s *kvTokenStore) DeleteOAuthToken(ctx context.Context, name string) error {
	return s.kv.DeleteKV(ctx, tokenKey(name))
}

func (s *kvTokenStore) GetClientRegistration(ctx context.Context, name string) (*ClientRegistration, error) {
	var reg ClientRegistration
	if err := s.kv.GetKV(ctx, clientKey(name), &reg); err != nil {
		return nil, err
	}
	return &reg, nil
}

func (s *kvTokenStore) SetClientRegistration(ctx context.Context, name string, reg *ClientRegistration) error {
	b, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("mcpoauth: marshal registration: %w", err)
	}
	return s.kv.SetKV(ctx, clientKey(name), json.RawMessage(b))
}

func tokenKey(name string) string  { return "mcp_oauth_token:" + name }
func clientKey(name string) string { return "mcp_oauth_client:" + name }
