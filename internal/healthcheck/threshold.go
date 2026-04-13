package healthcheck

import (
	"fmt"
	"strconv"
	"strings"
)

// EvaluateThreshold parses a threshold expression (e.g. "> 0.95", "< 500",
// "== 0") and evaluates it against the given value. Returns whether the
// condition is met (healthy) and a human-readable summary.
//
// Supported operators: >, >=, <, <=, ==, !=
func EvaluateThreshold(value float64, expr string) (healthy bool, summary string, err error) {
	op, threshold, err := parseThreshold(expr)
	if err != nil {
		return false, "", err
	}

	switch op {
	case ">":
		healthy = value > threshold
	case ">=":
		healthy = value >= threshold
	case "<":
		healthy = value < threshold
	case "<=":
		healthy = value <= threshold
	case "==":
		healthy = value == threshold
	case "!=":
		healthy = value != threshold
	default:
		return false, "", fmt.Errorf("unsupported operator %q", op)
	}

	status := "healthy"
	if !healthy {
		status = "unhealthy"
	}
	summary = fmt.Sprintf("%s: %.4g %s %s (threshold: %s)",
		status, value, op, formatFloat(threshold), expr)
	return healthy, summary, nil
}

func parseThreshold(expr string) (op string, threshold float64, err error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", 0, fmt.Errorf("empty threshold expression")
	}

	for _, candidate := range []string{">=", "<=", "!=", "==", ">", "<"} {
		if strings.HasPrefix(expr, candidate) {
			op = candidate
			rest := strings.TrimSpace(expr[len(candidate):])
			threshold, err = strconv.ParseFloat(rest, 64)
			if err != nil {
				return "", 0, fmt.Errorf("invalid threshold value %q in expression %q: %w", rest, expr, err)
			}
			return op, threshold, nil
		}
	}
	return "", 0, fmt.Errorf("no operator found in threshold expression %q (expected one of >, >=, <, <=, ==, !=)", expr)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
