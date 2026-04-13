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

func WithUser(ctx context.Context, user *LoginUser) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

func GetUser(ctx context.Context) *LoginUser {
	return ctx.Value(contextKey{}).(*LoginUser)
}

func GetUserID(ctx context.Context) string {
	u := GetUser(ctx)
	if u == nil {
		return ""
	}
	return u.UserID
}
