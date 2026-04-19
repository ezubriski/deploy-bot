package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
)

// OIDCConfig captures what the middleware needs to validate bearer ID
// tokens forwarded from the UI. The UI performs the authorization code
// flow against IssuerURL and attaches the resulting id_token to API
// calls as "Authorization: Bearer <id_token>".
//
// Audience must match the "aud" claim the IdP mints onto the token —
// typically the UI's client_id.
type OIDCConfig struct {
	IssuerURL string
	Audience  string
}

// Claims is the subset of the validated ID token surfaced to handlers.
// Extend as authorization decisions grow — this stub only pulls what a
// read-only UI plausibly needs to display/filter.
type Claims struct {
	Subject string
	Email   string
	Groups  []string
}

type ctxKey int

const userKey ctxKey = 1

// UserFromContext returns the validated claims attached by OIDC middleware,
// or nil if the request bypassed middleware (e.g. in tests).
func UserFromContext(ctx context.Context) *Claims {
	v, _ := ctx.Value(userKey).(*Claims)
	return v
}

// OIDC builds middleware that validates an ID token on each request
// against the issuer's JWKS and the configured audience. Unauthenticated
// or invalid-token requests get a JSON 401; no redirect is performed
// because this API is not user facing.
//
// The provider discovery round-trip happens once at construction time,
// so a misconfigured issuer fails fast at process start rather than
// per request.
func OIDC(ctx context.Context, cfg OIDCConfig, log *zap.Logger) (func(http.Handler) http.Handler, error) {
	if log == nil {
		log = zap.NewNop()
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.Audience})

	reject := func(w http.ResponseWriter, code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(errResp{Error: msg}); err != nil {
			log.Debug("write auth rejection", zap.Error(err))
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				reject(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			tok, err := verifier.Verify(r.Context(), raw)
			if err != nil {
				log.Debug("id token rejected", zap.Error(err))
				reject(w, http.StatusUnauthorized, "invalid token")
				return
			}
			var parsed struct {
				Email  string   `json:"email"`
				Groups []string `json:"groups"`
			}
			if err := tok.Claims(&parsed); err != nil {
				log.Debug("parse claims", zap.Error(err))
			}
			ctx := context.WithValue(r.Context(), userKey, &Claims{
				Subject: tok.Subject,
				Email:   parsed.Email,
				Groups:  parsed.Groups,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}, nil
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if tok == "" {
		return "", false
	}
	return tok, true
}
