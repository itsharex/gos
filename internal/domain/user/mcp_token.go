package user

import (
	"context"
	"errors"
	"time"
)

var ErrMCPTokenNotFound = errors.New("mcp token not found")

// MCPToken contains only token metadata on read. TokenHash is used for insertion
// and is never serialized or returned by the SQL repository's read methods.
type MCPToken struct {
	ID          string
	UserID      string
	Label       string
	TokenPrefix string
	TokenHash   string `json:"-"`
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// MCPTokenRepository is kept separate from login sessions: personal tokens do
// not inherit the replace-on-login semantics of UserSession.
type MCPTokenRepository interface {
	InitSchema(ctx context.Context) error
	CreateMCPToken(ctx context.Context, item MCPToken) error
	GetMCPTokenByHash(ctx context.Context, hash string) (MCPToken, error)
	ListMCPTokensByUser(ctx context.Context, userID string) ([]MCPToken, error)
	RevokeMCPToken(ctx context.Context, userID, tokenID string, revokedAt time.Time) error
}
