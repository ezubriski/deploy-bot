// Package api provides a read-only HTTP API over deploy-bot's durable
// state. It is intended to be consumed by a separate UI running in the
// same Kubernetes cluster — the API itself handles no user-facing login
// flow; authentication is delegated to the UI's OIDC provider and
// forwarded to the API as a bearer ID token (see oidc.go).
//
// Responses mirror the shapes in internal/store to minimize translation;
// the /v1 path prefix is the stability contract. Schema changes that
// rename store fields must update handler projections here to keep the
// /v1 surface stable.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/store"
)

const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 500
)

// Reader is the subset of *store.Store this API depends on. Defined as
// an interface so tests can inject a fake without standing up Postgres.
type Reader interface {
	GetHistory(ctx context.Context, appFilter string, limit int) ([]store.HistoryEntry, error)
	GetAll(ctx context.Context) ([]*store.PendingDeploy, error)
	Get(ctx context.Context, org, repo string, prNumber int) (*store.PendingDeploy, error)
	FindHistoryBySHA(ctx context.Context, sha string) (*store.HistoryEntry, error)
}

// Server wires the reader and config holder to HTTP handlers. Construct
// with New; mount Routes() at /.
type Server struct {
	reader Reader
	cfg    *config.Holder
	log    *zap.Logger
}

func New(reader Reader, cfg *config.Holder, log *zap.Logger) *Server {
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{reader: reader, cfg: cfg, log: log}
}

// Routes returns the versioned route tree. The returned mux is suitable
// for wrapping in authentication middleware before mounting.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/apps", s.listApps)
	mux.HandleFunc("GET /v1/apps/{appEnv}/history", s.appHistory)
	mux.HandleFunc("GET /v1/apps/{appEnv}/pending", s.appPending)
	mux.HandleFunc("GET /v1/deploys/{org}/{repo}/{pr}", s.getDeploy)
	mux.HandleFunc("GET /v1/history", s.historyBySHA)
	return mux
}

type appView struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	FullName    string `json:"full_name"`
	Source      string `json:"source"`
	SourceRepo  string `json:"source_repo,omitempty"`
	AutoDeploy  bool   `json:"auto_deploy,omitempty"`
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	out := make([]appView, 0, len(cfg.Apps))
	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		src := "operator"
		if a.SourceRepo != "" {
			src = "repo"
		}
		out = append(out, appView{
			App:         a.App,
			Environment: a.Environment,
			FullName:    a.FullName(),
			Source:      src,
			SourceRepo:  a.SourceRepo,
			AutoDeploy:  a.AutoDeploy,
		})
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) appHistory(w http.ResponseWriter, r *http.Request) {
	appEnv := r.PathValue("appEnv")
	limit := parseLimit(r.URL.Query().Get("limit"))
	rows, err := s.reader.GetHistory(r.Context(), appEnv, limit)
	if err != nil {
		s.log.Error("get history", zap.Error(err), zap.String("app", appEnv))
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

// appPending filters the full pending table in process. The table is
// in-flight only (terminal rows move to history), so it is small enough
// that an in-process filter keeps the API decoupled from a
// store-level "by app" query that doesn't exist.
func (s *Server) appPending(w http.ResponseWriter, r *http.Request) {
	appEnv := r.PathValue("appEnv")
	rows, err := s.reader.GetAll(r.Context())
	if err != nil {
		s.log.Error("get pending", zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]*store.PendingDeploy, 0, len(rows))
	for _, d := range rows {
		if d.App == appEnv {
			out = append(out, d)
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDeploy(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	repo := r.PathValue("repo")
	pr, err := strconv.Atoi(r.PathValue("pr"))
	if err != nil || pr <= 0 {
		s.writeError(w, http.StatusBadRequest, "pr must be a positive integer")
		return
	}
	d, err := s.reader.Get(r.Context(), org, repo, pr)
	if err == nil {
		s.writeJSON(w, http.StatusOK, d)
		return
	}
	if errors.Is(err, store.ErrPendingNotFound) || errors.Is(err, pgx.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "deploy not found")
		return
	}
	s.log.Error("get deploy", zap.Error(err))
	s.writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) historyBySHA(w http.ResponseWriter, r *http.Request) {
	sha := r.URL.Query().Get("sha")
	if sha == "" {
		s.writeError(w, http.StatusBadRequest, "sha query parameter required")
		return
	}
	e, err := s.reader.FindHistoryBySHA(r.Context(), sha)
	if err != nil {
		s.log.Error("find history by sha", zap.Error(err))
		s.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if e == nil {
		s.writeError(w, http.StatusNotFound, "no history for sha")
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}

func parseLimit(raw string) int {
	if raw == "" {
		return defaultHistoryLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}

type errResp struct {
	Error string `json:"error"`
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("write json response", zap.Error(err))
	}
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(errResp{Error: msg}); err != nil {
		s.log.Debug("write error response", zap.Error(err))
	}
}
