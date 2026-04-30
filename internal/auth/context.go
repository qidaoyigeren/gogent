package auth

import "context"

// LoginUser represents the authenticated user info.
type LoginUser struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Avatar   string `json:"avatar,omitempty"`
}

type contextKey struct{}

// WithUser stores LoginUser into context.
func WithUser(ctx context.Context, user *LoginUser) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

// GetUser retrieves LoginUser from context. Returns nil if not present.
func GetUser(ctx context.Context) *LoginUser {
	u, _ := ctx.Value(contextKey{}).(*LoginUser)
	return u
}

// GetUserID retrieves user ID from context. Returns "" if not present.
func GetUserID(ctx context.Context) string {
	u := GetUser(ctx)
	if u == nil {
		return ""
	}
	return u.UserID
}
