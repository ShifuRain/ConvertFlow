package api

import "net/http"

// Recoverer exposes the panic-recovery middleware to black-box tests.
func Recoverer(next http.Handler) http.Handler { return recoverer(next) }

// ValidID exposes the job-id validator to black-box tests.
func ValidID(id string) bool { return validID(id) }

// RoutePatterns lists every registered "METHOD /path" route (with {id}-style wildcards).
func RoutePatterns() []string { return routePatterns() }

// CSPOrigin exposes the upload-origin validator used for the Content-Security-Policy.
func CSPOrigin(raw string) (string, bool) { return cspOrigin(raw) }

// BaseCSP is the policy without an upload origin.
const BaseCSP = csp
