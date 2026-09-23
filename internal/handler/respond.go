package handler

import (
	"encoding/json"
	"net/http"
)

// errorResponse is the body of every error response, so clients decode a
// single shape regardless of which endpoint failed.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON encodes body as JSON and writes it with the given status.
//
// The body is marshalled before any header is written, so an encoding failure
// still produces a clean 500 rather than a success status with a truncated
// body. Errors from writing to the connection are ignored: they mean the
// client has gone away and there is nobody left to report them to.
func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

// writeError writes message as a JSON error body with the given status.
// message is sent to the client verbatim, so it must never contain internal
// details such as database errors.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
