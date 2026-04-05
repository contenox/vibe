package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/libauth"
	"github.com/golang-jwt/jwt/v5"
)

const (
	adminUsername = "admin"
	adminPassword = "admin"
	adminUserID   = "admin-123"
)

var jwtSecret = []byte("your-secret-key-change-this") // TODO: load from env in production

type tokenClaims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type SimpleTokenManager struct {
	secret []byte
	ttl    time.Duration
}

func NewSimpleTokenManager(ttl time.Duration) *SimpleTokenManager {
	return &SimpleTokenManager{
		secret: jwtSecret,
		ttl:    ttl,
	}
}

func (tm *SimpleTokenManager) Login(ctx context.Context, username, password string) (middleware.LoginResponse, error) {
	if username != adminUsername || password != adminPassword {
		return middleware.LoginResponse{}, fmt.Errorf("invalid credentials")
	}
	// Pass nil for permissions (interface zero value)
	token, expiresAt, err := tm.CreateAuthToken(ctx, adminUserID, nil)
	if err != nil {
		return middleware.LoginResponse{}, err
	}
	return middleware.LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		UserID:    adminUserID,
		Username:  adminUsername,
	}, nil
}

func (tm *SimpleTokenManager) CreateAuthToken(ctx context.Context, subject string, perms libauth.Authz) (string, time.Time, error) {
	expiresAt := time.Now().Add(tm.ttl)
	claims := tokenClaims{
		UserID:   subject,
		Username: adminUsername,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(tm.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return tokenStr, expiresAt, nil
}

func (tm *SimpleTokenManager) ValidateAuthToken(ctx context.Context) (context.Context, error) {
	tokenStr, ok := ctx.Value(libauth.ContextTokenKey).(string)
	if !ok || tokenStr == "" {
		return ctx, errors.New("no token in context")
	}
	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return tm.secret, nil
	})
	if err != nil || !token.Valid {
		return ctx, fmt.Errorf("invalid token: %w", err)
	}
	ctx = context.WithValue(ctx, contextKeyClaims, claims)
	return ctx, nil
}

func (tm *SimpleTokenManager) SetToken(ctx context.Context, tokenString string) (context.Context, error) {
	return context.WithValue(ctx, libauth.ContextTokenKey, tokenString), nil
}

func (tm *SimpleTokenManager) RefreshToken(ctx context.Context, tokenString string, withGracePeriod *time.Duration) (string, bool, time.Time, error) {
	claims := &tokenClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return tm.secret, nil
	})
	if err != nil || !token.Valid {
		return "", false, time.Time{}, err
	}
	now := time.Now()
	if claims.ExpiresAt != nil && now.Before(claims.ExpiresAt.Time) {
		if withGracePeriod == nil || claims.ExpiresAt.Time.Sub(now) > *withGracePeriod {
			return "", false, time.Time{}, nil
		}
	}
	// Pass nil for permissions
	newToken, newExp, err := tm.CreateAuthToken(ctx, claims.UserID, nil)
	if err != nil {
		return "", false, time.Time{}, err
	}
	return newToken, true, newExp, nil
}

// AuthZReader methods
func (tm *SimpleTokenManager) GetIdentity(ctx context.Context) (string, error) {
	claims, err := getClaims(ctx)
	if err != nil {
		return "", err
	}
	return claims.UserID, nil
}

func (tm *SimpleTokenManager) GetUsername(ctx context.Context) (string, error) {
	claims, err := getClaims(ctx)
	if err != nil {
		return "", err
	}
	return claims.Username, nil
}

// GetPermissions returns nil because we don't manage permissions.
func (tm *SimpleTokenManager) GetPermissions(ctx context.Context) (libauth.Authz, error) {
	return nil, nil // zero value for interface
}

func (tm *SimpleTokenManager) GetTokenString(ctx context.Context) (string, error) {
	token, ok := ctx.Value(libauth.ContextTokenKey).(string)
	if !ok || token == "" {
		return "", errors.New("token not found in context")
	}
	return token, nil
}

func (tm *SimpleTokenManager) GetExpiresAt(ctx context.Context) (time.Time, error) {
	claims, err := getClaims(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if claims.ExpiresAt == nil {
		return time.Time{}, errors.New("no expiry in claims")
	}
	return claims.ExpiresAt.Time, nil
}

type contextKey string

const contextKeyClaims contextKey = "token_claims"

func getClaims(ctx context.Context) (*tokenClaims, error) {
	claims, ok := ctx.Value(contextKeyClaims).(*tokenClaims)
	if !ok || claims == nil {
		return nil, errors.New("claims not found in context")
	}
	return claims, nil
}
