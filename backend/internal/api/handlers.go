package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"unichatgo/internal/auth"
	"unichatgo/internal/models"
	"unichatgo/internal/redis"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/worker"
)

type WorkerManager interface {
	InitSession(worker.SessionRequest) (*models.Session, error)
	Stream(worker.StreamRequest) (*models.Message, string, error)
	ResetUser(userID int64)
	Purge(userID, sessionID int64)
	InvalidateTempFiles(userID, sessionID int64)
}

// Handler wires HTTP routes to the assistant service and manages per-user AI workers.
type Handler struct {
	assistant *assistant.Service
	auth      *auth.Service
	workers   WorkerManager
	fileBase  string
	fileTTL   time.Duration
}

// NewHandler constructs a Handler instance.
func NewHandler(service *assistant.Service, authService *auth.Service, cfg worker.DispatcherConfig, fileBase string, fileTTL time.Duration, cacheClient *redis.Client) *Handler {
	return &Handler{
		assistant: service,
		auth:      authService,
		workers:   worker.NewManager(service, cfg, cacheClient),
		fileBase:  fileBase,
		fileTTL:   fileTTL,
	}
}

// check token userID is match with param userID
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
	userRoutes.POST("/uploads", h.filesUpload)
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
	SessionID int64   `json:"session_id"`
	Content   string  `json:"content"`
	ModelType string  `json:"model_type"`
	Provider  string  `json:"provider"`
	FileIDs   []int64 `json:"file_ids"`
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
	files, err := h.resolveTempFiles(c.Request.Context(), userID, req.SessionID, req.FileIDs)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
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
	// SSE Request construction
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
	// Send request
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
	// Stream sent
	streamReq := worker.StreamRequest{
		SessionRequest: worker.SessionRequest{
			Context:   streamCtx,
			UserID:    userID,
			SessionID: req.SessionID,
			Provider:  req.Provider,
			Model:     req.ModelType,
			Token:     token,
			Files:     files,
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

func (h *Handler) resolveTempFiles(ctx context.Context, userID, sessionID int64, fileIDs []int64) ([]*models.TempFile, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(fileIDs))
	ids := make([]int64, 0, len(fileIDs))
	for _, id := range fileIDs {
		if id <= 0 {
			return nil, errors.New("invalid file id")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	files, err := h.assistant.GetTempFilesByIDs(ctx, userID, sessionID, ids)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, sql.ErrNoRows
	}
	byID := make(map[int64]*models.TempFile, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}
	ordered := make([]*models.TempFile, 0, len(ids))
	for _, id := range ids {
		f, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("file id %d not found", id)
		}
		ordered = append(ordered, f)
	}
	return ordered, nil
}

const (
	maxUploadBytes   = 10 << 20 // 10 MB
	userStorageLimit = 50 << 20 // 50 MB per user
)

var allowedContentTypes = []string{
	"text/plain",
	"text/markdown",
	"application/pdf",
	"application/json",
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"image/",
}

func isAllowedContentType(ct string) bool {
	for _, allowed := range allowedContentTypes {
		if strings.HasPrefix(ct, allowed) {
			return true
		}
	}
	return false
}

func (h *Handler) filesUpload(c *gin.Context) {
	userID, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	if err := c.Request.ParseMultipartForm(maxUploadBytes); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart form"})
		return
	}
	sessionVal := c.PostForm("session_id")
	sessionID, err := strconv.ParseInt(sessionVal, 10, 64)
	if err != nil || sessionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session_id"})
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	if file.Size > maxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
		return
	}
	usage, err := h.assistant.TempStorageUsage(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "calculate usage failed"})
		return
	}
	if usage+file.Size > userStorageLimit {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "storage quota exceeded"})
		return
	}
	f, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "open file failed"})
		return
	}
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	_ = f.Close()
	contentType := http.DetectContentType(buf[:n])
	if !isAllowedContentType(contentType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported file type"})
		return
	}
	filename := filepath.Base(file.Filename)
	destDir, destPath, finalName := h.getUniqueFilePath(userID, sessionID, filename)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create directory failed"})
		return
	}
	if err := c.SaveUploadedFile(file, destPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save file failed"})
		return
	}
	fileID, err := h.assistant.RecordTempFile(c.Request.Context(), userID, sessionID, finalName, destPath, contentType, file.Size, h.fileTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "record file failed"})
		return
	}
	h.workers.InvalidateTempFiles(userID, sessionID)
	c.JSON(http.StatusCreated, gin.H{
		"file_id":   fileID,
		"file_name": finalName,
		"size":      file.Size,
		"mime":      contentType,
		"used":      usage + file.Size,
		"limit":     userStorageLimit,
	})
}

func (h *Handler) getFilePath(userID, sessionID int64, filename string) (string, string) {
	destDir := filepath.Join(h.fileBase, strconv.FormatInt(userID, 10), strconv.FormatInt(sessionID, 10))
	destPath := filepath.Join(destDir, filename)
	return destDir, destPath
}

func (h *Handler) getUniqueFilePath(userID, sessionID int64, filename string) (string, string, string) {
	destDir, destPath := h.getFilePath(userID, sessionID, filename)
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		return destDir, destPath, filename
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for idx := 1; idx <= 1000; idx++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, idx, ext)
		dir, path := h.getFilePath(userID, sessionID, candidate)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return dir, path, candidate
		}
	}
	return destDir, filepath.Join(destDir, fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext)), fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext)
}
