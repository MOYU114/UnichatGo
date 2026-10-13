package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"unichatgo/internal/auth"
	"unichatgo/internal/config"
	"unichatgo/internal/execution"
	"unichatgo/internal/models"
	"unichatgo/internal/service/assistant"
)

func (h *Handler) EnableAsync(repo *execution.Repository, cfg *config.Config) {
	h.execution = repo
	h.asyncConfig = cfg
}

// NewAsyncHandler does not start the legacy pool/cache listener.
func NewAsyncHandler(service *assistant.Service, authService *auth.Service, repo *execution.Repository, cfg *config.Config) *Handler {
	return &Handler{assistant: service, auth: authService, execution: repo, asyncConfig: cfg}
}

func asyncError(c *gin.Context, err error) {
	code, status := "dependency_unavailable", http.StatusServiceUnavailable
	switch {
	case errors.Is(err, execution.ErrNotFound):
		code, status = "not_found", 404
	case errors.Is(err, execution.ErrConflict):
		code, status = "payload_conflict", 409
	case errors.Is(err, execution.ErrBlocked):
		code, status = "session_uncertain", 409
	case errors.Is(err, execution.ErrLimit):
		code, status = "quota_exceeded", 429
	case errors.Is(err, execution.ErrBusy), errors.Is(err, execution.ErrStale):
		code, status = "execution_busy", 409
	case errors.Is(err, errInputBudget):
		code, status = "token_budget_exceeded", http.StatusUnprocessableEntity
	}
	var pe *execution.ProviderError
	if errors.As(err, &pe) {
		code, status = "unsupported_input", http.StatusUnprocessableEntity
	}
	c.JSON(status, gin.H{"error": code})
}

var errInputBudget = errors.New("input cannot fit configured token budget")

func canonicalInput(req inputRequest) (inputRequest, string, error) {
	if len(req.FileIDs) > 0 && req.AttachmentNamespace != "shared-v1" {
		return req, "", errors.New("attachment_namespace_required")
	}
	req.AttachmentNamespace = "shared-v1"
	req.Content = strings.TrimSpace(req.Content)
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.ModelType = strings.TrimSpace(req.ModelType)
	if req.SessionID <= 0 || req.Content == "" || len(req.Content) > 65536 || req.ClientMsgID == "" || len(req.ClientMsgID) > 128 || len(req.FileIDs) > 16 {
		return req, "", errors.New("invalid_input")
	}
	for _, id := range req.FileIDs {
		if id <= 0 {
			return req, "", errors.New("invalid_attachment")
		}
	}
	sort.Slice(req.FileIDs, func(i, j int) bool { return req.FileIDs[i] < req.FileIDs[j] })
	ids := make([]int64, 0, len(req.FileIDs))
	for _, id := range req.FileIDs {
		if len(ids) == 0 || ids[len(ids)-1] != id {
			ids = append(ids, id)
		}
	}
	req.FileIDs = ids
	semantic := req
	semantic.ClientMsgID = ""
	b, _ := json.Marshal(semantic)
	hash := sha256.Sum256(b)
	return req, hex.EncodeToString(hash[:]), nil
}

func (h *Handler) captureAsync(c *gin.Context) {
	uid, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	var req inputRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid_input"})
		return
	}
	req, hash, err := canonicalInput(req)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	// Authorization and replay happen before secrets, defaults or files are resolved.
	receipt, err := h.execution.Lookup(c.Request.Context(), uid, req.SessionID, req.ClientMsgID, hash)
	if err == nil {
		c.JSON(200, receipt)
		return
	}
	if !errors.Is(err, execution.ErrNotFound) {
		asyncError(c, err)
		return
	}
	if h.asyncConfig.Async.Paused {
		c.JSON(503, gin.H{"error": "acceptance_paused"})
		return
	}
	provider, ok := h.asyncConfig.Providers[req.Provider]
	if !ok {
		c.JSON(400, gin.H{"error": "unsupported_provider"})
		return
	}
	model := req.ModelType
	if model == "" {
		model = provider.Model
	}
	call := execution.CallInput{Provider: req.Provider, Model: model, BaseURL: provider.BaseURL, MaxOutputTokens: execution.DefaultMaxOutputTokens, Messages: []models.Message{{Role: models.RoleUser, Content: req.Content}}}
	for _, id := range req.FileIDs {
		a, err := h.execution.LoadAttachment(c.Request.Context(), uid, req.SessionID, id)
		if err != nil {
			asyncError(c, err)
			return
		}
		call.Attachments = append(call.Attachments, a)
	}
	if err = execution.ValidateCallInput(call); err != nil {
		c.JSON(400, gin.H{"error": "unsupported_input"})
		return
	}
	token, err := h.assistant.EnsureAIReady(c.Request.Context(), uid, req.Provider)
	if err != nil {
		if errors.Is(err, assistant.ErrProviderTokenMissing) {
			c.JSON(400, gin.H{"error": "provider_token_unavailable"})
		} else {
			asyncError(c, err)
		}
		return
	}
	// Scope identical credentials together even if used by different BYOK users.
	scope := sha256.Sum256([]byte(req.Provider + "\x00" + provider.BaseURL + "\x00" + token))
	if provider.QuotaGroup != "" {
		scope = sha256.Sum256([]byte("configured-account-group\x00" + provider.QuotaGroup))
	}
	snapshot, _ := json.Marshal(execution.Snapshot{Provider: req.Provider, Model: model, BaseURL: provider.BaseURL, Token: token, MaxOutputTokens: execution.DefaultMaxOutputTokens})
	sealed, err := h.assistant.SealExecution(string(snapshot))
	if err != nil {
		asyncError(c, err)
		return
	}
	budget := h.asyncConfig.Async.WithQuotaDefaults().TPM
	receipt, replay, err := h.execution.AcceptChecked(c.Request.Context(), execution.Input{UserID: uid, SessionID: req.SessionID, ClientID: req.ClientMsgID, PayloadHash: hash, Content: req.Content, Provider: req.Provider, Model: model, Snapshot: sealed, QuotaScope: hex.EncodeToString(scope[:]), AttachmentIDs: req.FileIDs}, func(history []models.Message, attachments []execution.Attachment) error {
		// This callback performs no I/O. The transaction rechecks the winning key
		// before computing budget, so concurrent retries still return its receipt.
		call.Messages, call.Attachments = history, attachments
		tokens, err := execution.EstimateTokens(call)
		if err != nil {
			return err
		}
		if tokens > budget {
			return errInputBudget
		}
		return nil
	})
	if err != nil {
		asyncError(c, err)
		return
	}
	status := http.StatusAccepted
	if replay {
		status = 200
	}
	c.JSON(status, receipt)
}

func (h *Handler) resolveAsync(c *gin.Context) {
	uid, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	if h.execution == nil {
		c.JSON(404, gin.H{"error": "async_disabled"})
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Action != "abandon" || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 1000 {
		c.JSON(400, gin.H{"error": "explicit_abandon_reason_required"})
		return
	}
	e, err := h.execution.Get(c.Request.Context(), c.Param("request_id"))
	if err != nil {
		asyncError(c, err)
		return
	}
	if e.UserID != uid {
		c.JSON(404, gin.H{"error": "not_found"})
		return
	}
	if err = h.execution.Resolve(c.Request.Context(), uid, e.SessionID, e.ID, req.Reason); err != nil {
		asyncError(c, err)
		return
	}
	c.JSON(200, gin.H{"request_id": e.ID, "state": "failed", "error_code": "abandoned_uncertain"})
}

func (h *Handler) startAsync(c *gin.Context) {
	uid, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	var req struct {
		SessionID int64 `json:"session_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.SessionID < 0 {
		c.JSON(400, gin.H{"error": "invalid_input"})
		return
	}
	if req.SessionID == 0 {
		if h.asyncConfig.Async.Paused {
			c.JSON(503, gin.H{"error": "acceptance_paused"})
			return
		}
		s, err := h.assistant.CreateSession(c.Request.Context(), uid, "New Conversation")
		if err != nil {
			asyncError(c, err)
			return
		}
		c.JSON(202, gin.H{"sessionId": s.ID, "userId": s.UserID, "title": s.Title, "createdAt": s.CreatedAt, "updatedAt": s.UpdatedAt})
		return
	}
	s, _, err := h.assistant.GetSessionWithMessages(c.Request.Context(), uid, req.SessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(404, gin.H{"error": "not_found"})
		} else {
			asyncError(c, err)
		}
		return
	}
	c.JSON(202, gin.H{"sessionId": s.ID, "userId": s.UserID, "title": s.Title, "createdAt": s.CreatedAt, "updatedAt": s.UpdatedAt})
}

func (h *Handler) uploadAsync(c *gin.Context) {
	uid, ok := h.authorizedUserID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 9<<20)
	if err := c.Request.ParseMultipartForm(1 << 20); err != nil {
		c.JSON(413, gin.H{"error": "upload_too_large"})
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	sid, err := strconv.ParseInt(c.PostForm("session_id"), 10, 64)
	if err != nil || sid <= 0 {
		c.JSON(400, gin.H{"error": "invalid_session"})
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(400, gin.H{"error": "file_required"})
		return
	}
	f, err := fh.Open()
	if err != nil {
		asyncError(c, err)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		c.JSON(413, gin.H{"error": "upload_too_large"})
		return
	}
	mime := strings.Split(http.DetectContentType(b), ";")[0]
	if err = execution.ValidateAttachments([]execution.Attachment{{Name: fh.Filename, MIME: mime, Data: b}}); err != nil {
		c.JSON(400, gin.H{"error": "unsupported_attachment"})
		return
	}
	a, err := h.execution.Upload(c.Request.Context(), uid, sid, fh.Filename, mime, b)
	if err != nil {
		asyncError(c, err)
		return
	}
	c.JSON(201, gin.H{"file_id": a.ID, "file_name": a.Name, "size": a.Size, "mime": a.MIME, "sha256": a.SHA256, "created_at": time.Now().UTC()})
}
