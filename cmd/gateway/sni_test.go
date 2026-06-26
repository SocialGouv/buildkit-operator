package main

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// A real ClientHello (produced by crypto/tls) must yield its SNI through our passthrough parser,
// and the raw bytes must be the handshake record we replay to the backend.
func TestPeekClientHelloSNI_RealHandshake(t *testing.T) {
	const sni = "buildkitd-p1a2b3c4d5e6f7a8.builds.example.com"
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	go func() {
		// tls.Client writes the ClientHello, then blocks on the never-completed handshake.
		_ = tls.Client(cli, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	}()

	_ = srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, raw, err := peekClientHelloSNI(srv)
	if err != nil {
		t.Fatalf("peekClientHelloSNI: %v", err)
	}
	if got != sni {
		t.Errorf("SNI = %q, want %q", got, sni)
	}
	if len(raw) < 5 || raw[0] != 0x16 {
		t.Errorf("raw bytes are not a TLS handshake record (len=%d)", len(raw))
	}
}

func TestBackendFor(t *testing.T) {
	// Multi-domain: one gateway fronts two client populations (a public domain + a CI-platform domain).
	g := &gateway{domains: []string{"builds.example.com", "ci.example.org"}, namespace: "buildkit-operator", port: 1234}

	for _, sni := range []string{"buildkitd-pabc.builds.example.com", "buildkitd-pabc.ci.example.org"} {
		if got, err := g.backendFor(sni); err != nil || got != "buildkitd-pabc.buildkit-operator.svc:1234" {
			t.Errorf("backendFor(%q) = %q, %v; want buildkitd-pabc.buildkit-operator.svc:1234", sni, got, err)
		}
	}
	// Defense in depth: reject foreign domains, non-daemon names, and cross-namespace traversal.
	for _, bad := range []string{
		"evil.example.com",
		"buildkitd-x.other.com",
		"notadaemon.builds.example.com",
		"buildkitd-x.ns.builds.example.com",
	} {
		if _, err := g.backendFor(bad); err == nil {
			t.Errorf("backendFor(%q) should be rejected", bad)
		}
	}
}
