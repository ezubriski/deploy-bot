package healthcheck

import (
	"testing"
)

func TestEvaluateThreshold(t *testing.T) {
	tests := []struct {
		name    string
		value   float64
		expr    string
		healthy bool
		wantErr bool
	}{
		{"gt healthy", 0.96, "> 0.95", true, false},
		{"gt unhealthy", 0.94, "> 0.95", false, false},
		{"gt boundary", 0.95, "> 0.95", false, false},
		{"gte healthy", 0.95, ">= 0.95", true, false},
		{"gte unhealthy", 0.94, ">= 0.95", false, false},
		{"lt healthy", 100, "< 500", true, false},
		{"lt unhealthy", 600, "< 500", false, false},
		{"lte healthy", 500, "<= 500", true, false},
		{"lte unhealthy", 501, "<= 500", false, false},
		{"eq healthy", 0, "== 0", true, false},
		{"eq unhealthy", 1, "== 0", false, false},
		{"ne healthy", 1, "!= 0", true, false},
		{"ne unhealthy", 0, "!= 0", false, false},
		{"negative threshold", -5, "> -10", true, false},
		{"decimal threshold", 0.001, "< 0.01", true, false},
		{"empty expr", 0, "", false, true},
		{"no operator", 0, "0.95", false, true},
		{"bad value", 0, "> abc", false, true},
		{"spaces around", 0.96, ">  0.95", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthy, summary, err := EvaluateThreshold(tt.value, tt.expr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvaluateThreshold() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if healthy != tt.healthy {
				t.Errorf("EvaluateThreshold() healthy = %v, want %v (summary: %s)", healthy, tt.healthy, summary)
			}
			if summary == "" {
				t.Error("EvaluateThreshold() returned empty summary")
			}
		})
	}
}

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		expr    string
		wantOp  string
		wantVal float64
		wantErr bool
	}{
		{"> 0.95", ">", 0.95, false},
		{">=100", ">=", 100, false},
		{"< 500", "<", 500, false},
		{"<=0", "<=", 0, false},
		{"== 42", "==", 42, false},
		{"!= 0", "!=", 0, false},
		{">= -3.14", ">=", -3.14, false},
		{"", "", 0, true},
		{"abc", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			op, val, err := parseThreshold(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseThreshold() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if op != tt.wantOp {
				t.Errorf("op = %q, want %q", op, tt.wantOp)
			}
			if val != tt.wantVal {
				t.Errorf("val = %v, want %v", val, tt.wantVal)
			}
		})
	}
}
