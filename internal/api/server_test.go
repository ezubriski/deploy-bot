package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/store"
)

type fakeReader struct {
	history    []store.HistoryEntry
	historyErr error

	pending    []*store.PendingDeploy
	pendingErr error

	getResult *store.PendingDeploy
	getErr    error

	shaResult *store.HistoryEntry
	shaErr    error

	// Capture of the last call so tests can assert argument passthrough.
	lastHistoryApp   string
	lastHistoryLimit int
	lastGetOrg       string
	lastGetRepo      string
	lastGetPR        int
	lastSHA          string
}

func (f *fakeReader) GetHistory(_ context.Context, appFilter string, limit int) ([]store.HistoryEntry, error) {
	f.lastHistoryApp = appFilter
	f.lastHistoryLimit = limit
	return f.history, f.historyErr
}
func (f *fakeReader) GetAll(_ context.Context) ([]*store.PendingDeploy, error) {
	return f.pending, f.pendingErr
}
func (f *fakeReader) Get(_ context.Context, org, repo string, pr int) (*store.PendingDeploy, error) {
	f.lastGetOrg = org
	f.lastGetRepo = repo
	f.lastGetPR = pr
	return f.getResult, f.getErr
}
func (f *fakeReader) FindHistoryBySHA(_ context.Context, sha string) (*store.HistoryEntry, error) {
	f.lastSHA = sha
	return f.shaResult, f.shaErr
}

func newTestServer(t *testing.T, r Reader, apps []config.AppConfig) *Server {
	t.Helper()
	cfg := &config.Config{Apps: apps}
	holder := config.NewHolder(cfg, "")
	return New(r, holder, nil)
}

func doRequest(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return v
}

func TestListApps(t *testing.T) {
	apps := []config.AppConfig{
		{App: "a", Environment: "dev"},
		{App: "b", Environment: "prod", AutoDeploy: true, SourceRepo: "org/b"},
	}
	s := newTestServer(t, &fakeReader{}, apps)

	rec := doRequest(t, s, "GET", "/v1/apps")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200: %s", rec.Code, rec.Body.String())
	}
	got := decode[[]appView](t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("len = %d want 2", len(got))
	}
	if got[0].FullName != "a-dev" || got[0].Source != "operator" {
		t.Errorf("first app = %+v", got[0])
	}
	if got[1].FullName != "b-prod" || got[1].Source != "repo" || got[1].SourceRepo != "org/b" || !got[1].AutoDeploy {
		t.Errorf("second app = %+v", got[1])
	}
}

func TestAppHistory(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		reader    *fakeReader
		wantCode  int
		wantApp   string
		wantLimit int
	}{
		{
			name:      "default limit",
			path:      "/v1/apps/myapp-dev/history",
			reader:    &fakeReader{history: []store.HistoryEntry{{App: "myapp-dev"}}},
			wantCode:  200,
			wantApp:   "myapp-dev",
			wantLimit: defaultHistoryLimit,
		},
		{
			name:      "custom limit",
			path:      "/v1/apps/myapp-dev/history?limit=10",
			reader:    &fakeReader{},
			wantCode:  200,
			wantApp:   "myapp-dev",
			wantLimit: 10,
		},
		{
			name:      "clamped to max",
			path:      "/v1/apps/myapp-dev/history?limit=99999",
			reader:    &fakeReader{},
			wantCode:  200,
			wantApp:   "myapp-dev",
			wantLimit: maxHistoryLimit,
		},
		{
			name:      "bad limit falls back to default",
			path:      "/v1/apps/myapp-dev/history?limit=nope",
			reader:    &fakeReader{},
			wantCode:  200,
			wantApp:   "myapp-dev",
			wantLimit: defaultHistoryLimit,
		},
		{
			name:     "reader error",
			path:     "/v1/apps/myapp-dev/history",
			reader:   &fakeReader{historyErr: errors.New("boom")},
			wantCode: 500,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.reader, nil)
			rec := doRequest(t, s, "GET", tc.path)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode != 200 {
				return
			}
			if tc.reader.lastHistoryApp != tc.wantApp {
				t.Errorf("app filter = %q want %q", tc.reader.lastHistoryApp, tc.wantApp)
			}
			if tc.reader.lastHistoryLimit != tc.wantLimit {
				t.Errorf("limit = %d want %d", tc.reader.lastHistoryLimit, tc.wantLimit)
			}
		})
	}
}

func TestAppPendingFilters(t *testing.T) {
	reader := &fakeReader{
		pending: []*store.PendingDeploy{
			{App: "myapp-dev", PRNumber: 1},
			{App: "other-prod", PRNumber: 2},
			{App: "myapp-dev", PRNumber: 3},
		},
	}
	s := newTestServer(t, reader, nil)

	rec := doRequest(t, s, "GET", "/v1/apps/myapp-dev/pending")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	got := decode[[]*store.PendingDeploy](t, rec.Body.Bytes())
	if len(got) != 2 || got[0].PRNumber != 1 || got[1].PRNumber != 3 {
		t.Errorf("filtered = %+v", got)
	}
}

func TestAppPendingReaderError(t *testing.T) {
	s := newTestServer(t, &fakeReader{pendingErr: errors.New("boom")}, nil)
	rec := doRequest(t, s, "GET", "/v1/apps/myapp-dev/pending")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d want 500", rec.Code)
	}
}

func TestGetDeploy(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		reader   *fakeReader
		wantCode int
	}{
		{
			name:     "found",
			path:     "/v1/deploys/acme/gitops/42",
			reader:   &fakeReader{getResult: &store.PendingDeploy{PRNumber: 42}},
			wantCode: 200,
		},
		{
			name:     "not found via sentinel",
			path:     "/v1/deploys/acme/gitops/42",
			reader:   &fakeReader{getErr: store.ErrPendingNotFound},
			wantCode: 404,
		},
		{
			name:     "bad pr number",
			path:     "/v1/deploys/acme/gitops/zero",
			reader:   &fakeReader{},
			wantCode: 400,
		},
		{
			name:     "negative pr",
			path:     "/v1/deploys/acme/gitops/-3",
			reader:   &fakeReader{},
			wantCode: 400,
		},
		{
			name:     "reader error",
			path:     "/v1/deploys/acme/gitops/42",
			reader:   &fakeReader{getErr: errors.New("boom")},
			wantCode: 500,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.reader, nil)
			rec := doRequest(t, s, "GET", tc.path)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == 200 {
				if tc.reader.lastGetOrg != "acme" || tc.reader.lastGetRepo != "gitops" || tc.reader.lastGetPR != 42 {
					t.Errorf("pass-through: org=%q repo=%q pr=%d",
						tc.reader.lastGetOrg, tc.reader.lastGetRepo, tc.reader.lastGetPR)
				}
			}
		})
	}
}

func TestHistoryBySHA(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		reader   *fakeReader
		wantCode int
	}{
		{
			name:     "found",
			path:     "/v1/history?sha=abc123",
			reader:   &fakeReader{shaResult: &store.HistoryEntry{GitopsCommitSHA: "abc123"}},
			wantCode: 200,
		},
		{
			name:     "missing sha",
			path:     "/v1/history",
			reader:   &fakeReader{},
			wantCode: 400,
		},
		{
			name:     "not found",
			path:     "/v1/history?sha=nope",
			reader:   &fakeReader{shaResult: nil},
			wantCode: 404,
		},
		{
			name:     "reader error",
			path:     "/v1/history?sha=abc",
			reader:   &fakeReader{shaErr: errors.New("boom")},
			wantCode: 500,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.reader, nil)
			rec := doRequest(t, s, "GET", tc.path)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestRoutesRejectWrongMethod(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, nil)
	rec := doRequest(t, s, "POST", "/v1/apps")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", rec.Code)
	}
}

func TestResponsesAreJSON(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, nil)
	rec := doRequest(t, s, "GET", "/v1/apps")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q want application/json", ct)
	}
}

func TestErrorResponseShape(t *testing.T) {
	s := newTestServer(t, &fakeReader{}, nil)
	rec := doRequest(t, s, "GET", "/v1/history")
	got := decode[errResp](t, rec.Body.Bytes())
	if got.Error == "" {
		t.Fatalf("error body empty")
	}
}

// Quick smoke ensuring parseLimit is pure (no logger coupling) — helps
// future callers feel safe using it from other contexts.
func TestParseLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", defaultHistoryLimit},
		{"0", defaultHistoryLimit},
		{"-1", defaultHistoryLimit},
		{"abc", defaultHistoryLimit},
		{"25", 25},
		{fmt.Sprintf("%d", maxHistoryLimit+1), maxHistoryLimit},
	}
	for _, tc := range cases {
		if got := parseLimit(tc.in); got != tc.want {
			t.Errorf("parseLimit(%q) = %d want %d", tc.in, got, tc.want)
		}
	}
}
