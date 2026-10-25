package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gos/internal/application/usecase"
	userdomain "gos/internal/domain/user"
	"gos/internal/infrastructure/persistence/sqlrepo"

	_ "modernc.org/sqlite"
)

func TestMCPTokenHandlerOwnerLifecycleAndNoSecretInList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	users := sqlrepo.NewUserRepository(db, "sqlite")
	if err := users.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	tokens := sqlrepo.NewMCPTokenRepository(db, "sqlite")
	if err := tokens.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u1", "u2"} {
		if err := users.CreateUser(ctx, userdomain.User{ID: id, Username: id, DisplayName: id,
			Role: userdomain.RoleNormal, Status: userdomain.StatusActive, PasswordHash: "x",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	manager := usecase.NewMCPTokenManager(tokens, users)
	ownerID := "u1"
	router := gin.New()
	router.Use(func(c *gin.Context) {
		setCurrentUser(c, userdomain.User{ID: ownerID})
		c.Next()
	})
	NewMCPTokenHandler(manager).RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodPost, "/me/mcp-tokens", bytes.NewBufferString(`{"name":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	router.ServeHTTP(created, req)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status=%d, cache_control=%q", created.Code, created.Header().Get("Cache-Control"))
	}
	var creation struct {
		Data struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Token       string `json:"token"`
			TokenPrefix string `json:"token_prefix"`
			ExpiredAt   string `json:"expired_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &creation); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(creation.Data.Token, usecase.MCPTokenPrefix) || creation.Data.ID == "" || creation.Data.Name != "client" || creation.Data.ExpiredAt == "" {
		t.Fatal("invalid MCP token create response")
	}
	if _, err := manager.ResolveUserByToken(ctx, creation.Data.Token); err != nil {
		t.Fatalf("created token cannot resolve: %v", err)
	}
	listed := httptest.NewRecorder()
	router.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/me/mcp-tokens", nil))
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), creation.Data.Token) || strings.Contains(listed.Body.String(), "token_hash") {
		t.Fatalf("list leaked raw token/hash: %s", listed.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil || len(list.Data) != 1 {
		t.Fatalf("list body=%s err=%v", listed.Body, err)
	}
	ownerID = "u2"
	foreign := httptest.NewRecorder()
	router.ServeHTTP(foreign, httptest.NewRequest(http.MethodGet, "/me/mcp-tokens", nil))
	if foreign.Code != http.StatusOK || strings.Contains(foreign.Body.String(), creation.Data.ID) {
		t.Fatalf("foreign list=%d %s", foreign.Code, foreign.Body)
	}
	foreignRevoke := httptest.NewRecorder()
	router.ServeHTTP(foreignRevoke, httptest.NewRequest(http.MethodDelete, "/me/mcp-tokens/"+creation.Data.ID, nil))
	if foreignRevoke.Code != http.StatusNotFound {
		t.Fatalf("foreign revoke=%d %s", foreignRevoke.Code, foreignRevoke.Body)
	}
	ownerID = "u1"
	revoked := httptest.NewRecorder()
	router.ServeHTTP(revoked, httptest.NewRequest(http.MethodDelete, "/me/mcp-tokens/"+creation.Data.ID, nil))
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("owner revoke=%d %s", revoked.Code, revoked.Body)
	}
	if _, err := manager.ResolveUserByToken(ctx, creation.Data.Token); err != userdomain.ErrMCPTokenNotFound {
		t.Fatalf("revoked token resolve=%v", err)
	}
}

func TestMCPTokenHandlerRequiresCurrentUserAndValidInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewMCPTokenHandler(nil).RegisterRoutes(router)
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/me/mcp-tokens", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	badRouter := gin.New()
	badRouter.Use(func(c *gin.Context) {
		setCurrentUser(c, userdomain.User{ID: "u1"})
		c.Next()
	})
	NewMCPTokenHandler(usecase.NewMCPTokenManager(nil, nil)).RegisterRoutes(badRouter)
	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/me/mcp-tokens", bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	badRouter.ServeHTTP(bad, req)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad body status=%d", bad.Code)
	}
}
