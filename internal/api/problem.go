package api

import (
	"encoding/json"
	"net/http"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an application/problem+json response. Detail must be a fixed, client-safe
// message: never include filesystem paths, SQL or other internal error text.
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "about:blank", Title: title, Status: status, Detail: detail})
}
