package main

import (
	"os"
	"strconv"
	"time"
)

// envOr returns the value of env var key, or def when unset/empty. Used to make
// every cobra flag default env-aware so the same binary is configured the same
// way from flags (local) or env (the pod spec).
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envDurationOr parses key as a Go duration (e.g. "15s"), falling back to def on
// unset or unparseable input.
func envDurationOr(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envFloatOr parses key as a float64, falling back to def on unset or
// unparseable input.
func envFloatOr(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
