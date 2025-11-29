package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CSRFMiddleware enforces double-submit CSRF protection for cookie-authenticated requests.
func (s *Service) CSRFMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requiresCSRFCheck(c.Request.Method) {
			c.Next()
			return
		}
		authHeader := c.GetHeader(s.headerName)
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			// Explicit bearer authorization is exempt from CSRF checks.
			c.Next()
			return
		}
		headerToken := c.GetHeader(s.csrfHeaderName)
		cookieToken, err := c.Cookie(s.csrfCookieName)
		if err != nil || headerToken == "" || cookieToken == "" || headerToken != cookieToken {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid csrf token"})
			return
		}
		c.Next()
	}
}

func requiresCSRFCheck(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
