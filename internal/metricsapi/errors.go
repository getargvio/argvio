package metricsapi

import (
	"encoding/json"
	"net/http"
)

// errorBody matches the shape documented in openapi/openapi.yaml's error
// response schema — kept intentionally simple (code/message) and
// consistent across every 4xx/5xx this server returns.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
