package mcp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// StaticTokenVerifier returns an auth.TokenVerifier that validates against a
// static bearer token. This is suitable for server-to-server auth where the
// token is shared via environment variable or secret manager.
func StaticTokenVerifier(token string) auth.TokenVerifier {
	return func(_ context.Context, candidate string, _ *http.Request) (*auth.TokenInfo, error) {
		if candidate != token {
			return nil, fmt.Errorf("invalid bearer token: %w", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{
			// Static tokens don't expire — set a far-future expiration to
			// satisfy the SDK's expiration check.
			Expiration: time.Now().Add(24 * 365 * 100 * time.Hour),
		}, nil
	}
}
