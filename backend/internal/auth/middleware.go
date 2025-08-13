package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	userIDContextKey    = "auth_user_id"
	authTokenContextKey = "auth_token"
)

// Middleware validates bearer tokens and stores the authenticated user in the context.
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authToken := s.extractToken(c)
		if authToken == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			return
		}
		userID, err := s.ValidateToken(c.Request.Context(), authToken)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.Set(userIDContextKey, userID)
		c.Set(authTokenContextKey, authToken)
		c.Next()
	}
}

// UserIDFromContext retrieves the authenticated user id from the gin context.
func UserIDFromContext(c *gin.Context) (int64, bool) {
	val, ok := c.Get(userIDContextKey)
	if !ok {
		return 0, false
	}
	userID, ok := val.(int64)
	return userID, ok
}

// TokenFromContext retrieves the bearer token captured by the middleware.
func AuthTokenFromContext(c *gin.Context) (string, bool) {
	val, ok := c.Get(authTokenContextKey)
	if !ok {
		return "", false
	}
	token, ok := val.(string)
	return token, ok
}

func (s *Service) extractToken(c *gin.Context) string {
	authHeader := c.GetHeader(s.headerName)
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	if token, err := c.Cookie(s.cookieName); err == nil && token != "" {
		return token
	}
	return ""
}
