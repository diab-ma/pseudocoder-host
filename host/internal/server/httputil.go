package server

import (
	"encoding/json"
	"net/http"
)

// writeJSON writes a JSON response with the given status code and body.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
