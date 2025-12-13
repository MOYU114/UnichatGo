package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"unichatgo/internal/auth"
	"unichatgo/internal/models"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/worker"
)

type WorkerManager interface {
	InitSession(worker.SessionRequest) (*models.Session, error)
	Stream(worker.StreamRequest) (*models.Message, string, error)
	ResetUser(userID int64)
	Purge(userID, sessionID int64)
}

// Handler wires HTTP routes to the assistant service and manages per-user AI workers.
type Handler struct {
	assistant *assistant.Service
	auth      *auth.Service
	workers   WorkerManager
}

// NewHandler constructs a Handler instance.
func NewHandler(service *assistant.Service, authService *auth.Service, cfg worker.DispatcherConfig) *Handler {
	return &Handler{
		assistant: service,
		auth:      authService,
		workers:   worker.NewManager(service, cfg),
	}
}

func (h *Handler) requirePathUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := auth.UserIDFromContext(c)
		if !ok || userID <= 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			return
		}
		paramID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || paramID <= 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
			return
		}
		if paramID != userID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user mismatch"})
			return
		}
		c.Next()
	}
}

func (h *Handler) authorizedUserID(c *gin.Context) (int64, bool) {
	userID, ok := auth.UserIDFromContext(c)
	if !ok || userID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return 0, false
	}
	return userID, true
}

// RegisterRoutes attaches all HTTP routes to the router.
func (h *Handler) RegisterRoutes(router *gin.Engine) {
	api := router.Group("/api")
	api.POST("/users/register", h.registerUser)
	api.POST("/users/login", h.loginUser)
	authMW := h.auth.Middleware()
	userRoutes := api.Group("/users/:id")
	userRoutes.Use(authMW, h.requirePathUser(), h.auth.CSRFMiddleware())
	userRoutes.POST("/token", h.setToken)
	userRoutes.GET("/token", h.listTokens)
	userRoutes.DELETE("/token", h.deleteToken)
	userRoutes.POST("/conversation/session-list", h.getSessionList)
	userRoutes.POST("/conversation/start", h.startConversation)
	userRoutes.DELETE("/conversation/sessions/:session_id", h.deleteSession)
	userRoutes.GET("/conversation/sessions/:session_id/messages", h.getSessionMessages)
	userRoutes.POST("/conversation/msg", h.captureInput)
	userRoutes.POST("/logout", h.logoutUser)
	userRoutes.DELETE("", h.deleteUser)
}

// User create&login interface
type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) registerUser(c *gin.Context) {
	var req credentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	user, err := h.assistant.RegisterUser(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, worker.ErrDispatcherBusy) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "server is busy, please retry"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":         user.ID,
		"username":   user.Username,
		"created_at": user.CreatedAt,
	})
}

func (h *Handler) loginUser(c *gin.Context) {
	var req credentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	user, err := h.assistant.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	authToken, err := h.auth.IssueToken(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue token failed"})
		return
	}
	csrfToken, err := h.auth.NewCSRFToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue token failed"})
		return
	}
	h.setAuthCookies(c, authToken, csrfToken)
	c.JSON(http.StatusOK, gin.H{
		"id":         user.ID,
		"username":   user.Username,
		"created_at": user.CreatedAt,
		"auth_token": authToken,
	})
}

func (h *Handler) getSessionList(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	seList, err := h.assistant.ListSessions(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(seList) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"session_list": make([]models.Session, 0),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session_list": seList,
	})
}

func (h *Handler) deleteSession(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	sessionID, err := strconv.ParseInt(c.Param("session_id"), 10, 64)
	if err != nil || sessionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	if err := h.assistant.DeleteSession(c.Request.Context(), userID, sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.workers.Purge(userID, sessionID)
	c.Status(http.StatusNoContent)
}

func (h *Handler) startConversation(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	var req struct {
		Provider  string `json:"provider"`
		SessionID int64  `json:"session_id"`
		ModelType string `json:"model_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	sessionID := req.SessionID
	if sessionID < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id cannot be negative"})
		return
	}
	modelType := strings.TrimSpace(req.ModelType)
	if modelType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_type is required"})
		return
	}
	token, err := h.assistant.EnsureAIReady(c.Request.Context(), userID, provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, err := h.workers.InitSession(worker.SessionRequest{
		Context:   c.Request.Context(),
		UserID:    userID,
		SessionID: sessionID,
		Provider:  provider,
		Model:     modelType,
		Token:     token,
	})
	if err != nil {
		if errors.Is(err, worker.ErrDispatcherBusy) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "server is busy, please retry"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"sessionId": session.ID,
		"userId":    session.UserID,
		"title":     session.Title,
		"createdAt": session.CreatedAt,
		"updatedAt": session.UpdatedAt,
	})
}

func (h *Handler) logoutUser(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	h.workers.ResetUser(userID)
	if authToken, ok := auth.AuthTokenFromContext(c); ok {
		_ = h.auth.RevokeToken(c.Request.Context(), authToken)
	}
	h.clearAuthCookies(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) deleteUser(c *gin.Context) {
	id, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	if err := h.auth.RevokeUserTokens(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.workers.ResetUser(id)
	if err := h.assistant.DeleteUser(c.Request.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.clearAuthCookies(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) getSessionMessages(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	sessionID, err := strconv.ParseInt(c.Param("session_id"), 10, 64)
	if err != nil || sessionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	session, messages, err := h.assistant.GetSessionWithMessages(c.Request.Context(), userID, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"session":  session,
		"messages": messages,
	})
}

// User input interface
type inputRequest struct {
	SessionID int64  `json:"session_id"`
	Content   string `json:"content"`
	ModelType string `json:"model_type"`
	Provider  string `json:"provider"`
}

func (h *Handler) captureInput(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	var req inputRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.SessionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}
	message, err := h.assistant.AppendMessageToSession(c.Request.Context(), userID, req.SessionID, models.RoleUser, req.Content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token, err := h.assistant.EnsureAIReady(c.Request.Context(), userID, req.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	streamCtx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	sendEvent := func(event string, payload interface{}) error {
		var data []byte
		switch v := payload.(type) {
		case string:
			data = []byte(v)
		default:
			var err error
			data, err = json.Marshal(v)
			if err != nil {
				return err
			}
		}
		if event != "" {
			if _, err := fmt.Fprintf(c.Writer, "event: %s\n", event); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := sendEvent("ack", gin.H{
		"message": gin.H{
			"id":         message.ID,
			"user_id":    message.UserID,
			"session_id": message.SessionID,
			"role":       message.Role,
			"content":    message.Content,
			"created_at": message.CreatedAt,
		},
	}); err != nil {
		return
	}

	streamReq := worker.StreamRequest{
		SessionRequest: worker.SessionRequest{
			Context:   streamCtx,
			UserID:    userID,
			SessionID: req.SessionID,
			Provider:  req.Provider,
			Model:     req.ModelType,
			Token:     token,
			Message:   message,
		},
		ChunkFn: func(chunk string) error {
			return sendEvent("stream", gin.H{"content": chunk})
		},
	}
	aiMessage, title, err := h.workers.Stream(streamReq)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, worker.ErrDispatcherBusy) {
			msg = "server is busy, please retry"
		}
		_ = sendEvent("error", gin.H{"message": msg})
		return
	}
	_, err = h.assistant.AppendMessageToSession(c.Request.Context(), aiMessage.UserID, aiMessage.SessionID, aiMessage.Role, aiMessage.Content)
	if err != nil {
		_ = sendEvent("error", gin.H{"message": err.Error()})
		return
	}
	payload := gin.H{
		"user_message": gin.H{
			"id":         message.ID,
			"user_id":    message.UserID,
			"session_id": message.SessionID,
			"role":       message.Role,
			"content":    message.Content,
			"created_at": message.CreatedAt,
		},
		"ai_message": gin.H{
			"id":         aiMessage.ID,
			"user_id":    aiMessage.UserID,
			"session_id": aiMessage.SessionID,
			"role":       aiMessage.Role,
			"content":    aiMessage.Content,
			"created_at": aiMessage.CreatedAt,
		},
	}
	if title != "" {
		payload["title"] = title
	}
	_ = sendEvent("done", payload)
	return
}

// handle api token
func (h *Handler) setToken(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Token    string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.assistant.SetUserToken(c.Request.Context(), userID, req.Provider, req.Token); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) listTokens(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	tokens, err := h.assistant.ListUserTokens(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

func (h *Handler) deleteToken(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.assistant.DeleteUserToken(c.Request.Context(), userID, req.Provider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.workers.ResetUser(userID)
	c.Status(http.StatusNoContent)
}

func (h *Handler) setAuthCookies(c *gin.Context, authToken, csrfToken string) {
	ttl := int(h.auth.TokenTTL().Seconds())
	if ttl <= 0 {
		ttl = 3600
	}
	secure := gin.Mode() == gin.ReleaseMode
	setCookie(c, &http.Cookie{
		Name:     h.auth.AuthCookieName(),
		Value:    authToken,
		MaxAge:   ttl,
		Path:     "/",
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	setCookie(c, &http.Cookie{
		Name:     h.auth.CSRFCookieName(),
		Value:    csrfToken,
		MaxAge:   ttl,
		Path:     "/",
		Secure:   secure,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) clearAuthCookies(c *gin.Context) {
	for _, name := range []string{h.auth.AuthCookieName(), h.auth.CSRFCookieName()} {
		setCookie(c, &http.Cookie{
			Name:     name,
			Value:    "",
			MaxAge:   -1,
			Path:     "/",
			Secure:   gin.Mode() == gin.ReleaseMode,
			HttpOnly: name == h.auth.AuthCookieName(),
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func setCookie(c *gin.Context, ck *http.Cookie) {
	if ck == nil {
		return
	}
	http.SetCookie(c.Writer, ck)
}
