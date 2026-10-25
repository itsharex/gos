package sqlrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	userdomain "gos/internal/domain/user"
)

// MCPTokenRepository only stores irreversible token hashes. InitSchema must
// run after the user schema initializer and before token routes are exposed.
type MCPTokenRepository struct {
	db       *sql.DB
	dbDriver string
}

var _ userdomain.MCPTokenRepository = (*MCPTokenRepository)(nil)

func NewMCPTokenRepository(db *sql.DB, dbDriver string) *MCPTokenRepository {
	return &MCPTokenRepository{db: db, dbDriver: strings.ToLower(strings.TrimSpace(dbDriver))}
}

func (r *MCPTokenRepository) InitSchema(ctx context.Context) error {
	if err := r.createMCPTokenSchema(ctx); err != nil {
		return err
	}
	return runSchemaMigrations(ctx, r.db, r.dbDriver, schemaMigration{
		Version:     "20261009_01_mcp_token",
		Description: "create personal MCP tokens with hashed secrets",
		Up:          r.createMCPTokenSchema,
	})
}

func (r *MCPTokenRepository) createMCPTokenSchema(ctx context.Context) error {
	var statements []string
	switch r.dbDriver {
	case "mysql":
		statements = []string{`CREATE TABLE IF NOT EXISTS sys_mcp_token (
	id VARCHAR(64) PRIMARY KEY,
	user_id VARCHAR(64) NOT NULL,
	label VARCHAR(100) NOT NULL,
	token_prefix VARCHAR(32) NOT NULL,
	token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	created_at BIGINT NOT NULL,
	expires_at BIGINT NOT NULL,
	revoked_at BIGINT NULL,
	UNIQUE KEY uk_mcp_token_hash (token_hash),
	KEY idx_mcp_token_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`}
	case "sqlite":
		statements = []string{
			`CREATE TABLE IF NOT EXISTS sys_mcp_token (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	label TEXT NOT NULL,
	token_prefix TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER NULL
);`,
			`CREATE INDEX IF NOT EXISTS idx_mcp_token_user_created ON sys_mcp_token (user_id, created_at);`,
		}
	default:
		return fmt.Errorf("unsupported db driver: %s", r.dbDriver)
	}
	for _, stmt := range statements {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("initialize MCP token schema: %w", err)
		}
	}
	return nil
}

var mcpTokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (r *MCPTokenRepository) CreateMCPToken(ctx context.Context, item userdomain.MCPToken) error {
	if !mcpTokenHashPattern.MatchString(item.TokenHash) {
		return errors.New("invalid MCP token hash")
	}
	const query = `INSERT INTO sys_mcp_token
		(id, user_id, label, token_prefix, token_hash, created_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL);`
	_, err := r.db.ExecContext(ctx, query, item.ID, item.UserID, item.Label, item.TokenPrefix,
		item.TokenHash, item.CreatedAt.UTC().UnixNano(), item.ExpiresAt.UTC().UnixNano())
	return err
}

func (r *MCPTokenRepository) GetMCPTokenByHash(ctx context.Context, hash string) (userdomain.MCPToken, error) {
	if !mcpTokenHashPattern.MatchString(hash) {
		return userdomain.MCPToken{}, userdomain.ErrMCPTokenNotFound
	}
	const query = `SELECT id, user_id, label, token_prefix, created_at, expires_at, revoked_at
		FROM sys_mcp_token WHERE token_hash = ?;`
	item, err := scanMCPToken(r.db.QueryRowContext(ctx, query, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return userdomain.MCPToken{}, userdomain.ErrMCPTokenNotFound
	}
	return item, err
}

func (r *MCPTokenRepository) ListMCPTokensByUser(ctx context.Context, userID string) ([]userdomain.MCPToken, error) {
	const query = `SELECT id, user_id, label, token_prefix, created_at, expires_at, revoked_at
		FROM sys_mcp_token WHERE user_id = ? ORDER BY created_at DESC, id DESC;`
	rows, err := r.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]userdomain.MCPToken, 0)
	for rows.Next() {
		item, scanErr := scanMCPToken(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// The owner condition in this single UPDATE prevents a read-then-write race and
// ensures that guessing another user's token ID never reveals its metadata.
func (r *MCPTokenRepository) RevokeMCPToken(ctx context.Context, userID, tokenID string, revokedAt time.Time) error {
	const query = `UPDATE sys_mcp_token SET revoked_at = ? WHERE user_id = ? AND id = ? AND revoked_at IS NULL;`
	result, err := r.db.ExecContext(ctx, query, revokedAt.UTC().UnixNano(), userID, tokenID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return userdomain.ErrMCPTokenNotFound
	}
	return nil
}

func scanMCPToken(s interface{ Scan(...any) error }) (userdomain.MCPToken, error) {
	var item userdomain.MCPToken
	var created, expires int64
	var revoked sql.NullInt64
	if err := s.Scan(&item.ID, &item.UserID, &item.Label, &item.TokenPrefix, &created, &expires, &revoked); err != nil {
		return userdomain.MCPToken{}, err
	}
	item.CreatedAt = time.Unix(0, created).UTC()
	item.ExpiresAt = time.Unix(0, expires).UTC()
	if revoked.Valid {
		at := time.Unix(0, revoked.Int64).UTC()
		item.RevokedAt = &at
	}
	return item, nil
}
