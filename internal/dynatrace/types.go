package dynatrace

// QueryRequest is the JSON body sent to the DQL query:execute endpoint.
type QueryRequest struct {
	Query                 string `json:"query"`
	DefaultTimeframeStart string `json:"defaultTimeframeStart,omitempty"`
	DefaultTimeframeEnd   string `json:"defaultTimeframeEnd,omitempty"`
}

// QueryResponse is the top-level response from the DQL query:execute endpoint.
type QueryResponse struct {
	State    string       `json:"state"`
	Result   *QueryResult `json:"result,omitempty"`
	Progress int          `json:"progress"`
	// RequestToken is used for polling long-running queries.
	RequestToken string `json:"requestToken,omitempty"`
}

// QueryResult holds the tabular result of a completed DQL query.
type QueryResult struct {
	Records []map[string]any `json:"records"`
	// Metadata contains column type information (not always needed).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// FirstNumericValue extracts the first numeric value from the first record.
// Returns 0 and false if no numeric value is found.
//
// Map iteration order is nondeterministic, so if the first record contains
// multiple numeric fields, which one is returned is arbitrary. DQL queries
// used for health checks should be written to return a single aggregated
// value (e.g. `| summarize avg(value)`) so this is not ambiguous in practice.
func (r *QueryResult) FirstNumericValue() (float64, bool) {
	if r == nil || len(r.Records) == 0 {
		return 0, false
	}
	for _, v := range r.Records[0] {
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		}
	}
	return 0, false
}
