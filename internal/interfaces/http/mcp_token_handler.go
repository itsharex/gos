package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gos/internal/application/usecase"
	userdomain "gos/internal/domain/user"
)

// MCPTokenHandler manages only the authenticated user's own credentials.
// RegisterRoutes must be called on the protected router after authMiddleware.
type MCPTokenHandler struct {
	manager *usecase.MCPTokenManager
}

func NewMCPTokenHandler(manager *usecase.MCPTokenManager) *MCPTokenHandler {
	return &MCPTokenHandler{manager: manager}
}

func (h *MCPTokenHandler) RegisterRoutes(router gin.IRouter) {
	router.POST("/me/mcp-tokens", h.Create)
	router.GET("/me/mcp-tokens", h.List)
	router.DELETE("/me/mcp-tokens/:id", h.Revoke)
}

type MCPTokenCreateRequest struct {
	Name          string `json:"name"`
	ExpiresInDays *int   `json:"expires_in_days"`
}

type MCPTokenMetadataResponse struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiredAt   time.Time  `json:"expired_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
}

type MCPTokenCreateResponse struct {
	MCPTokenMetadataResponse
	Token string `json:"token"`
}

func (h *MCPTokenHandler) Create(c *gin.Context) {
	owner, ok := h.currentUser(c)
	if !ok {
		return
	}
	var req MCPTokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	input := usecase.CreateMCPTokenInput{Label: req.Name}
	if req.ExpiresInDays != nil {
		input.ExpiresInDays = *req.ExpiresInDays
		if input.ExpiresInDays == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expires_in_days must be 1-365"})
			return
		}
	}
	issued, err := h.manager.Create(mcpTokenRequestContext(c), owner.ID, input)
	if err != nil {
		writeMCPTokenHTTPError(c, err)
		return
	}
	// Secret-bearing responses must not be cached by clients or proxies.
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusCreated, gin.H{"data": MCPTokenCreateResponse{
		MCPTokenMetadataResponse: toMCPTokenMetadata(issued.Item),
		Token:                    issued.Token,
	}})
}

func (h *MCPTokenHandler) List(c *gin.Context) {
	owner, ok := h.currentUser(c)
	if !ok {
		return
	}
	items, err := h.manager.List(mcpTokenRequestContext(c), owner.ID)
	if err != nil {
		writeMCPTokenHTTPError(c, err)
		return
	}
	data := make([]MCPTokenMetadataResponse, 0, len(items))
	for _, item := range items {
		data = append(data, toMCPTokenMetadata(item))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": data})
}

func (h *MCPTokenHandler) Revoke(c *gin.Context) {
	owner, ok := h.currentUser(c)
	if !ok {
		return
	}
	if err := h.manager.Revoke(mcpTokenRequestContext(c), owner.ID, c.Param("id")); err != nil {
		writeMCPTokenHTTPError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *MCPTokenHandler) currentUser(c *gin.Context) (userdomain.User, bool) {
	owner, ok := getCurrentUser(c)
	if !ok || owner.ID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return userdomain.User{}, false
	}
	if h == nil || h.manager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mcp token manager is not configured"})
		return userdomain.User{}, false
	}
	return owner, true
}

func toMCPTokenMetadata(item userdomain.MCPToken) MCPTokenMetadataResponse {
	return MCPTokenMetadataResponse{
		ID:          item.ID,
		Name:        item.Label,
		TokenPrefix: item.TokenPrefix,
		CreatedAt:   item.CreatedAt,
		ExpiredAt:   item.ExpiresAt,
		RevokedAt:   item.RevokedAt,
	}
}

func mcpTokenRequestContext(c *gin.Context) context.Context {
	ctx := c.Request.Context()
	// W3C traceparent is supplied by the caller or proxy; accept a valid 32-byte
	// trace id and generate one in the use case when this header is absent.
	parts := strings.Split(c.GetHeader("traceparent"), "-")
	if len(parts) == 4 && len(parts[0]) == 2 && len(parts[2]) == 16 && len(parts[3]) == 2 {
		return usecase.WithMCPTokenTraceID(ctx, parts[1])
	}
	return ctx
}

func writeMCPTokenHTTPError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, usecase.ErrInvalidInput), errors.Is(err, usecase.ErrInvalidID):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, userdomain.ErrMCPTokenNotFound), errors.Is(err, userdomain.ErrUserNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "mcp token not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}
}
