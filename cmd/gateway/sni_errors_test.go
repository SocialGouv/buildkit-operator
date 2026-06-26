package main

import (
	"bytes"
	"testing"
)

func TestEnvOr(t *testing.T) {
	if envOr("BKO_GW_UNSET", "def") != "def" {
		t.Error("unset should return default")
	}
	t.Setenv("BKO_GW_SET", "v")
	if envOr("BKO_GW_SET", "def") != "v" {
		t.Error("set should return value")
	}
}

// TestPeekClientHelloSNI_Errors covers the malformed-record branches: a short header, a non-handshake
// record type, a zero/oversized record length, and a body truncated against its declared length.
func TestPeekClientHelloSNI_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"short header", []byte{0x16, 0x03}},                              // < 5 bytes: ReadFull fails
		{"not handshake", []byte{0x15, 0x03, 0x01, 0x00, 0x02, 1}},        // 0x15 = alert, not 0x16
		{"zero length", []byte{0x16, 0x03, 0x01, 0x00, 0x00}},             // declared length 0
		{"truncated body", []byte{0x16, 0x03, 0x01, 0x00, 0x10, 1, 2, 3}}, // says 16, only 3 follow
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := peekClientHelloSNI(bytes.NewReader(tt.in)); err == nil {
				t.Errorf("peekClientHelloSNI(%v): want error, got nil", tt.in)
			}
		})
	}
}

// TestSniFromClientHello_Truncated covers the short-ClientHello rejection in the parser directly.
func TestSniFromClientHello_Truncated(t *testing.T) {
	if _, err := sniFromClientHello([]byte{0x02}); err == nil {
		t.Error("non-ClientHello msg_type: want error")
	}
	// Valid ClientHello msg_type+length header but body too short for version+random.
	if _, err := sniFromClientHello([]byte{0x01, 0x00, 0x00, 0x02, 0x03, 0x03}); err == nil {
		t.Error("truncated ClientHello: want error")
	}
}
