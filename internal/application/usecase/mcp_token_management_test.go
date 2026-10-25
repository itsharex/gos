package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	userdomain "gos/internal/domain/user"
)

type memoryMCPTokens struct {
	byHash map[string]userdomain.MCPToken
	fail   error
}

func (r *memoryMCPTokens) InitSchema(context.Context) error { return nil }
func (r *memoryMCPTokens) CreateMCPToken(_ context.Context, item userdomain.MCPToken) error {
	if r.fail != nil {
		return r.fail
	}
	r.byHash[item.TokenHash] = item
	return nil
}
func (r *memoryMCPTokens) GetMCPTokenByHash(_ context.Context, hash string) (userdomain.MCPToken, error) {
	if r.fail != nil {
		return userdomain.MCPToken{}, r.fail
	}
	item, ok := r.byHash[hash]
	if !ok {
		return item, userdomain.ErrMCPTokenNotFound
	}
	item.TokenHash = ""
	return item, nil
}
func (r *memoryMCPTokens) ListMCPTokensByUser(_ context.Context, id string) ([]userdomain.MCPToken, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	items := make([]userdomain.MCPToken, 0)
	for _, item := range r.byHash {
		if item.UserID == id {
			item.TokenHash = ""
			items = append(items, item)
		}
	}
	return items, nil
}
func (r *memoryMCPTokens) RevokeMCPToken(_ context.Context, userID, tokenID string, now time.Time) error {
	if r.fail != nil {
		return r.fail
	}
	for hash, item := range r.byHash {
		if item.ID == tokenID && item.UserID == userID && item.RevokedAt == nil {
			item.RevokedAt = &now
			r.byHash[hash] = item
			return nil
		}
	}
	return userdomain.ErrMCPTokenNotFound
}

type memoryMCPUsers map[string]userdomain.User

func (r memoryMCPUsers) GetUserByID(_ context.Context, id string) (userdomain.User, error) {
	item, ok := r[id]
	if !ok {
		return item, userdomain.ErrUserNotFound
	}
	return item, nil
}

func TestMCPTokenManagerLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	repo := &memoryMCPTokens{byHash: map[string]userdomain.MCPToken{}}
	users := memoryMCPUsers{
		"u1": {ID: "u1", Status: userdomain.StatusActive},
		"u2": {ID: "u2", Status: userdomain.StatusActive},
	}
	manager := NewMCPTokenManager(repo, users)
	manager.now = func() time.Time { return now }
	issued, err := manager.Create(ctx, "u1", CreateMCPTokenInput{Label: " my workstation "})
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.Token) != len(MCPTokenPrefix)+64 || !strings.HasPrefix(issued.Token, MCPTokenPrefix) {
		t.Fatalf("issued unexpected token format")
	}
	if issued.Item.TokenHash != "" || issued.Item.Label != "my workstation" || issued.Item.ExpiresAt.Sub(now) != 90*24*time.Hour {
		t.Fatalf("issued metadata=%+v", issued.Item)
	}
	hash := sha256.Sum256([]byte(issued.Token))
	stored, ok := repo.byHash[hex.EncodeToString(hash[:])]
	if !ok || stored.TokenHash != hex.EncodeToString(hash[:]) || stored.TokenPrefix != issued.Token[:len(MCPTokenPrefix)+8] {
		t.Fatal("token was not stored as a SHA-256 hash with safe display prefix")
	}
	resolved, err := manager.ResolveMCPToken(ctx, issued.Token)
	if err != nil || resolved.ID != "u1" {
		t.Fatalf("resolve=%+v err=%v", resolved, err)
	}
	for _, bad := range []string{"", "not-a-token", issued.Token + "0", strings.ToUpper(issued.Token)} {
		if _, err := manager.ResolveUserByToken(ctx, bad); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
			t.Fatalf("malformed token accepted: %v", err)
		}
	}
	listed, err := manager.List(ctx, "u1")
	if err != nil || len(listed) != 1 || listed[0].TokenHash != "" {
		t.Fatalf("owner list=%+v err=%v", listed, err)
	}
	other, err := manager.List(ctx, "u2")
	if err != nil || len(other) != 0 {
		t.Fatalf("other list=%+v err=%v", other, err)
	}
	if err := manager.Revoke(ctx, "u2", issued.Item.ID); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("cross-owner revoke=%v", err)
	}
	if _, err := manager.ResolveUserByToken(ctx, issued.Token); err != nil {
		t.Fatalf("foreign revoke changed token: %v", err)
	}
	if err := manager.Revoke(ctx, "u1", issued.Item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResolveUserByToken(ctx, issued.Token); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("revoked token resolve=%v", err)
	}
}

func TestMCPTokenManagerRejectsExpiryInactiveDeletedAndBadInput(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	repo := &memoryMCPTokens{byHash: map[string]userdomain.MCPToken{}}
	users := memoryMCPUsers{"u1": {ID: "u1", Status: userdomain.StatusActive}}
	manager := NewMCPTokenManager(repo, users)
	manager.now = func() time.Time { return now }
	for _, input := range []CreateMCPTokenInput{
		{}, {Label: strings.Repeat("a", 101)}, {Label: "line\nbreak"},
		{Label: "ok", ExpiresInDays: -1}, {Label: "ok", ExpiresInDays: 366},
	} {
		if _, err := manager.Create(ctx, "u1", input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad input %+v, err=%v", input, err)
		}
	}
	issued, err := manager.Create(ctx, "u1", CreateMCPTokenInput{Label: "cli", ExpiresInDays: 365})
	if err != nil || issued.Item.ExpiresAt.Sub(now) != 365*24*time.Hour {
		t.Fatalf("max expiry incorrect: err=%v", err)
	}
	users["u1"] = userdomain.User{ID: "u1", Status: userdomain.StatusInactive}
	if _, err := manager.ResolveUserByToken(ctx, issued.Token); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("inactive user resolve=%v", err)
	}
	if _, err := manager.Create(ctx, "u1", CreateMCPTokenInput{Label: "cli"}); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("inactive user create=%v", err)
	}
	delete(users, "u1")
	if _, err := manager.ResolveUserByToken(ctx, issued.Token); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("deleted user resolve=%v", err)
	}
	users["u1"] = userdomain.User{ID: "u1", Status: userdomain.StatusActive}
	manager.now = func() time.Time { return issued.Item.ExpiresAt }
	if _, err := manager.ResolveUserByToken(ctx, issued.Token); !errors.Is(err, userdomain.ErrMCPTokenNotFound) {
		t.Fatalf("expiry boundary resolve=%v", err)
	}
}

func TestMCPTokenManagerPropagatesStorageFailure(t *testing.T) {
	failure := errors.New("database unavailable")
	repo := &memoryMCPTokens{byHash: map[string]userdomain.MCPToken{}, fail: failure}
	manager := NewMCPTokenManager(repo, memoryMCPUsers{"u1": {ID: "u1", Status: userdomain.StatusActive}})
	if _, err := manager.Create(context.Background(), "u1", CreateMCPTokenInput{Label: "cli"}); !errors.Is(err, failure) {
		t.Fatalf("create err=%v", err)
	}
	if _, err := manager.List(context.Background(), "u1"); !errors.Is(err, failure) {
		t.Fatalf("list err=%v", err)
	}
}
