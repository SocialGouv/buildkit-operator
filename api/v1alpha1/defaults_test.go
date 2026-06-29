package v1alpha1

import (
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDefaultsMatchMarkers parses the +kubebuilder:default markers from buildproject_types.go and
// asserts ApplyDefaults() reproduces exactly those values. This is what lets the two coexist without
// drift: change a marker without changing the constant (or vice-versa) and this fails.
func TestDefaultsMatchMarkers(t *testing.T) {
	src, err := os.ReadFile("buildproject_types.go")
	if err != nil {
		t.Fatalf("read types: %v", err)
	}

	// Map the JSON field name -> the literal in its +kubebuilder:default marker. RE2 has no
	// backreferences, so capture the raw value and strip any surrounding quotes in code.
	markerRe := regexp.MustCompile(`\+kubebuilder:default=(\S+)`)
	jsonRe := regexp.MustCompile("`json:\"([a-zA-Z]+)")
	lines := strings.Split(string(src), "\n")
	markers := map[string]string{}
	pending := ""
	for _, ln := range lines {
		if m := markerRe.FindStringSubmatch(ln); m != nil {
			pending = strings.Trim(m[1], `"`)
			continue
		}
		// Keep pending across intervening markers (e.g. a +kubebuilder:validation line) until the
		// field's json tag — the default marker immediately precedes its field, so it binds to the
		// next json tag we see.
		if pending != "" {
			if m := jsonRe.FindStringSubmatch(ln); m != nil {
				markers[m[1]] = pending
				pending = ""
			}
		}
	}

	// All four defaulted fields must be discovered (guards against a marker being moved/renamed).
	// storageClass intentionally has NO fixed default (operator-configured via buildd --default-storage-class).
	for _, f := range []string{"cacheVolumeGi", "tier", "idleTimeoutSec", "securityProfile"} {
		if _, ok := markers[f]; !ok {
			t.Fatalf("no +kubebuilder:default marker found for field %q", f)
		}
	}

	var bp BuildProject
	bp.ApplyDefaults()
	s := reflect.ValueOf(bp.Spec)

	fieldByJSON := map[string]string{
		"cacheVolumeGi":   "CacheVolumeGi",
		"tier":            "Tier",
		"idleTimeoutSec":  "IdleTimeoutSec",
		"securityProfile": "SecurityProfile",
	}
	for jsonName, want := range markers {
		goField, ok := fieldByJSON[jsonName]
		if !ok {
			continue // a defaulted field we don't assert on (none today)
		}
		got := s.FieldByName(goField)
		switch got.Kind() {
		case reflect.String:
			if got.String() != want {
				t.Errorf("%s: ApplyDefaults=%q marker=%q", goField, got.String(), want)
			}
		case reflect.Int32, reflect.Int:
			w, err := strconv.Atoi(want)
			if err != nil {
				t.Fatalf("%s marker %q not an int: %v", goField, want, err)
			}
			if got.Int() != int64(w) {
				t.Errorf("%s: ApplyDefaults=%d marker=%d", goField, got.Int(), w)
			}
		default:
			t.Fatalf("%s: unhandled kind %s", goField, got.Kind())
		}
	}
}
