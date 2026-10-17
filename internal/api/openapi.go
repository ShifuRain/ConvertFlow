package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiJSON []byte

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiJSON)
}
