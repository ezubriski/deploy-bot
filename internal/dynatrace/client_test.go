package dynatrace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestQueryDQL_ImmediateSuccess(t *testing.T) {
	result := QueryResponse{
		State: "SUCCEEDED",
		Result: &QueryResult{
			Records: []map[string]any{
				{"avg_response_time": 42.5},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != dqlQueryPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("unexpected content-type: %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}))
	defer srv.Close()

	c := &Client{
		httpClient:     srv.Client(),
		environmentURL: srv.URL,
		log:            zaptest.NewLogger(t),
	}

	got, err := c.QueryDQL(context.Background(), `fetch metrics | summarize avg(value)`)
	if err != nil {
		t.Fatalf("QueryDQL() error = %v", err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got.Records))
	}
	v, ok := got.FirstNumericValue()
	if !ok {
		t.Fatal("expected numeric value")
	}
	if v != 42.5 {
		t.Errorf("value = %v, want 42.5", v)
	}
}

func TestQueryDQL_AsyncPolling(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == dqlQueryPath:
			json.NewEncoder(w).Encode(QueryResponse{
				State:        "RUNNING",
				Progress:     50,
				RequestToken: "tok-123",
			})
		case r.URL.Path == pollPath && n <= 3:
			json.NewEncoder(w).Encode(QueryResponse{
				State:        "RUNNING",
				Progress:     75,
				RequestToken: "tok-123",
			})
		case r.URL.Path == pollPath:
			json.NewEncoder(w).Encode(QueryResponse{
				State: "SUCCEEDED",
				Result: &QueryResult{
					Records: []map[string]any{{"value": 99.0}},
				},
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{
		httpClient:     srv.Client(),
		environmentURL: srv.URL,
		log:            zaptest.NewLogger(t),
	}

	got, err := c.QueryDQL(context.Background(), `fetch metrics`)
	if err != nil {
		t.Fatalf("QueryDQL() error = %v", err)
	}
	v, ok := got.FirstNumericValue()
	if !ok {
		t.Fatal("expected numeric value")
	}
	if v != 99.0 {
		t.Errorf("value = %v, want 99.0", v)
	}
	if callCount.Load() < 3 {
		t.Errorf("expected at least 3 API calls (1 execute + 2 polls), got %d", callCount.Load())
	}
}

func TestQueryDQL_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("access denied"))
	}))
	defer srv.Close()

	c := &Client{
		httpClient:     srv.Client(),
		environmentURL: srv.URL,
		log:            zaptest.NewLogger(t),
	}

	_, err := c.QueryDQL(context.Background(), `fetch metrics`)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestQueryResult_FirstNumericValue(t *testing.T) {
	tests := []struct {
		name    string
		result  *QueryResult
		wantVal float64
		wantOK  bool
	}{
		{"nil result", nil, 0, false},
		{"empty records", &QueryResult{Records: nil}, 0, false},
		{"no numeric", &QueryResult{Records: []map[string]any{{"name": "foo"}}}, 0, false},
		{"float64", &QueryResult{Records: []map[string]any{{"v": 3.14}}}, 3.14, true},
		{"int", &QueryResult{Records: []map[string]any{{"v": 42}}}, 42, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := tt.result.FirstNumericValue()
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if v != tt.wantVal {
				t.Errorf("value = %v, want %v", v, tt.wantVal)
			}
		})
	}
}
