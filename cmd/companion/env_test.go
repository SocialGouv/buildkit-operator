package main

import (
	"testing"
	"time"
)

// TestEnvOr covers the string env fallback: set/non-empty returns the value, unset or empty returns def.
func TestEnvOr(t *testing.T) {
	const key = "BKO_TEST_ENVOR"
	if got := envOr(key, "fallback"); got != "fallback" {
		t.Errorf("unset: envOr = %q, want fallback", got)
	}
	t.Setenv(key, "")
	if got := envOr(key, "fallback"); got != "fallback" {
		t.Errorf("empty: envOr = %q, want fallback", got)
	}
	t.Setenv(key, "value")
	if got := envOr(key, "fallback"); got != "value" {
		t.Errorf("set: envOr = %q, want value", got)
	}
}

// TestEnvDurationOr covers parse / fallback-on-unparseable / fallback-on-unset for durations.
func TestEnvDurationOr(t *testing.T) {
	const key = "BKO_TEST_ENVDUR"
	def := 15 * time.Second
	if got := envDurationOr(key, def); got != def {
		t.Errorf("unset: envDurationOr = %v, want %v", got, def)
	}
	t.Setenv(key, "30s")
	if got := envDurationOr(key, def); got != 30*time.Second {
		t.Errorf("valid: envDurationOr = %v, want 30s", got)
	}
	t.Setenv(key, "not-a-duration")
	if got := envDurationOr(key, def); got != def {
		t.Errorf("unparseable: envDurationOr = %v, want fallback %v", got, def)
	}
}

// TestEnvFloatOr covers parse / fallback-on-unparseable / fallback-on-unset for floats.
func TestEnvFloatOr(t *testing.T) {
	const key = "BKO_TEST_ENVFLOAT"
	if got := envFloatOr(key, 0.85); got != 0.85 {
		t.Errorf("unset: envFloatOr = %v, want 0.85", got)
	}
	t.Setenv(key, "0.5")
	if got := envFloatOr(key, 0.85); got != 0.5 {
		t.Errorf("valid: envFloatOr = %v, want 0.5", got)
	}
	t.Setenv(key, "nope")
	if got := envFloatOr(key, 0.85); got != 0.85 {
		t.Errorf("unparseable: envFloatOr = %v, want fallback 0.85", got)
	}
}
