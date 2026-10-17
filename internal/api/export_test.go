package api

import "net/http"

// Recoverer exposes the panic-recovery middleware to black-box tests.
func Recoverer(next http.Handler) http.Handler { return recoverer(next) }

// ValidID exposes the job-id validator to black-box tests.
func ValidID(id string) bool { return validID(id) }

// RoutePatterns lists every registered "METHOD /path" route (with {id}-style wildcards).
func RoutePatterns() []string { return routePatterns() }
