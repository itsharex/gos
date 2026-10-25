package sqlrepo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	userdomain "gos/internal/domain/user"

	_ "modernc.org/sqlite"
)

func TestMCPTokenRepositoryMigrationHashOnlyAndOwnerRevoke(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewMCPTokenRepository(db, "sqlite")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := repo.InitSchema(ctx); err != nil {
			t.Fatalf("InitSchema(%d): %v", i, err)
		}
	}
	assertSchemaMigrationCount(t, db, "20261009_01_mcp_token", 1)
	now := time.Unix(1_800_000_000, 0).UTC()
	item := userdomain.MCPToken{
		ID: "mcp-1", UserID: "user-1", Label: "home", TokenPrefix: "gos_mcp_abcd1234",
		TokenHash: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour),
	}
	if err := repo.CreateMCPToken(ctx, item); err != nil {
		t.Fatal(err)
	}
	invalid := item
	invalid.ID = "mcp-raw"
	invalid.TokenHash = "gos_mcp_plaintext"
	if err := repo.CreateMCPToken(ctx, invalid); err == nil {
		t.Fatal("repository stored an invalid plaintext token in the hash column")
	}
	// Only the SHA-256 hash is searchable; no secret or plaintext column exists.
	columns, err := NewUserRepository(db, "sqlite").sqliteTableColumns(ctx, "sys_mcp_token")
	if err != nil {
		t.Fatal(err)
	}
	for _, secretColumn := range []string{"token", "access_token", "secret", "raw_token"} {
		if _, exists := columns[secretColumn]; exists {
			t.Fatalf("plaintext column %q exists", secretColumn)
		}
	}
	got, err := repo.GetMCPTokenByHash(ctx, item.TokenHash)
	if err != nil || got.UserID != item.UserID || got.TokenHash != "" || !got.ExpiresAt.Equal(item.ExpiresAt) {
		t.Fatalf("GetMCPTokenByHash got=%+v err=%v", got, err)
	}
	if _, err := repo.GetMCPTokenByHash(ctx, "gos_mcp_raw"); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("plaintext lookup err=%v", err)
	}
	if err := repo.RevokeMCPToken(ctx, "user-2", item.ID, now); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("cross-owner revoke err=%v", err)
	}
	if err := repo.RevokeMCPToken(ctx, item.UserID, item.ID, now); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if err := repo.RevokeMCPToken(ctx, item.UserID, item.ID, now); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("double revoke err=%v", err)
	}
	list, err := repo.ListMCPTokensByUser(ctx, item.UserID)
	if err != nil || len(list) != 1 || list[0].RevokedAt == nil || list[0].TokenHash != "" {
		t.Fatalf("owner list=%+v err=%v", list, err)
	}
	foreignList, err := repo.ListMCPTokensByUser(ctx, "user-2")
	if err != nil || len(foreignList) != 0 {
		t.Fatalf("foreign list=%+v err=%v", foreignList, err)
	}
}

func TestMCPTokenRepositoryRejectsUnsupportedDriver(t *testing.T) {
	repo := NewMCPTokenRepository(nil, "unknown")
	if err := repo.InitSchema(context.Background()); err == nil {
		t.Fatal("expected unsupported driver error")
	}
}
