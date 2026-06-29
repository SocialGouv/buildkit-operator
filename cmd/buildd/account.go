package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
)

func writeJSON(log logr.Logger, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Status + headers are already committed, so we can't change the response — but a swallowed
		// encode error leaves the client with a truncated body and no signal in our logs.
		log.Error(err, "encode JSON response failed")
	}
}

// clientIP returns the caller address for audit logs (best-effort: X-Forwarded-For first hop when set
// behind the LB, else the TCP peer). The host only — no port — keeps log lines stable.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
