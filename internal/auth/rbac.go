package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Role represents an RBAC role.
type Role string

const (
	RoleAdmin   Role = "admin"
	RoleTrader  Role = "trader"
	RoleViewer  Role = "viewer"
	RoleAuditor Role = "auditor"
)

// roleOrder defines privilege hierarchy (higher index = more privileged).
var roleOrder = map[Role]int{
	RoleAdmin:   4,
	RoleTrader:  3,
	RoleViewer:  2,
	RoleAuditor: 1,
}

// AtLeast returns true if r has at least the privilege level of required.
func (r Role) AtLeast(required Role) bool {
	return roleOrder[r] >= roleOrder[required]
}

// APIKey is a validated API key with associated metadata stored in context.
type APIKey struct {
	ID        string
	TenantID  string
	Name      string
	KeyPrefix string
	Role      Role
}

type contextKey int

const ctxKey contextKey = 0

// WithAPIKey stores the validated APIKey in the request context.
func WithAPIKey(ctx context.Context, key *APIKey) context.Context {
	return context.WithValue(ctx, ctxKey, key)
}

// FromContext retrieves the APIKey from the context (nil if not present).
func FromContext(ctx context.Context) *APIKey {
	k, _ := ctx.Value(ctxKey).(*APIKey)
	return k
}

// GenerateKey creates a new random API key.
// Returns the full key (for one-time display to user), prefix (display), and SHA-256 hash (storage).
func GenerateKey() (fullKey, prefix, keyHash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	fullKey = "ce_" + base64.RawURLEncoding.EncodeToString(b)
	// prefix = "ce_" + first 8 chars of base64 body
	if len(fullKey) > 11 {
		prefix = fullKey[:11]
	} else {
		prefix = fullKey
	}
	sum := sha256.Sum256([]byte(fullKey))
	keyHash = hex.EncodeToString(sum[:])
	return
}

// HashKey returns the SHA-256 hex hash of an arbitrary key string.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ExtractBearer extracts a Bearer token from Authorization header or ?token= query param.
func ExtractBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// ClientIP extracts the best-effort client IP from a request.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	// Strip port from RemoteAddr.
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// RequireRole returns an error if the key does not meet the required role.
func RequireRole(key *APIKey, required Role) error {
	if key == nil {
		return fmt.Errorf("not authenticated")
	}
	if !key.Role.AtLeast(required) {
		return fmt.Errorf("role %s required, have %s", required, key.Role)
	}
	return nil
}
