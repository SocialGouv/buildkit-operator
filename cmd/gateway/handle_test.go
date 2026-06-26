package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"
)

// runHandle runs g.handle(server-side conn) while a peer goroutine drives the client side, and
// returns once handle has returned (it always closes its conn). It fails if handle hangs.
func runHandle(t *testing.T, g *gateway, drivePeer func(peer net.Conn)) {
	t.Helper()
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() { g.handle(srv); close(done) }()
	if drivePeer != nil {
		go drivePeer(cli)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return")
	}
	_ = cli.Close()
}

// TestHandle_MaxConnsReject: a saturated conn limiter rejects immediately without reading the conn.
func TestHandle_MaxConnsReject(t *testing.T) {
	full := make(chan struct{}, 1)
	full <- struct{}{} // saturate
	g := &gateway{maxConns: full, domains: []string{"builds.example.com"}}
	runHandle(t, g, nil) // no peer needed: handle bails before peeking
}

// TestHandle_PeekError: a non-handshake first record makes peekClientHelloSNI fail and handle return.
func TestHandle_PeekError(t *testing.T) {
	g := &gateway{domains: []string{"builds.example.com"}, dialTO: 500 * time.Millisecond}
	runHandle(t, g, func(peer net.Conn) {
		_, _ = peer.Write([]byte{0x00, 0x01, 0x02, 0x03, 0x04}) // not 0x16 handshake
		_ = peer.Close()
	})
}

// TestHandle_BackendRejected: a valid ClientHello whose SNI is outside the gateway domains is rejected
// by backendFor, so handle returns without dialing.
func TestHandle_BackendRejected(t *testing.T) {
	g := &gateway{domains: []string{"builds.example.com"}, namespace: "ns", port: 1234, dialTO: 500 * time.Millisecond}
	runHandle(t, g, func(peer net.Conn) {
		_ = tls.Client(peer, &tls.Config{ServerName: "evil.attacker.com", InsecureSkipVerify: true}).Handshake()
	})
}

// TestServeHealth: /healthz and /readyz return 200, and cancelling the context shuts the server down.
func TestServeHealth(t *testing.T) {
	// Grab a free port, then release it for serveHealth to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	g := &gateway{}
	done := make(chan struct{})
	go func() { g.serveHealth(ctx, addr); close(done) }()

	// Poll until the server accepts, then assert both endpoints.
	var resp *http.Response
	for i := 0; i < 100; i++ {
		resp, err = http.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health server never came up: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if r, err := http.Get("http://" + addr + "/readyz"); err == nil {
		if r.StatusCode != http.StatusOK {
			t.Errorf("/readyz = %d, want 200", r.StatusCode)
		}
		_ = r.Body.Close()
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveHealth did not shut down on context cancel")
	}
}

// TestHandle_DialFails: an in-domain daemon SNI resolves to an in-cluster .svc address that does not
// exist in the test env, so net.DialTimeout fails and handle returns (covers the dial-error branch).
func TestHandle_DialFails(t *testing.T) {
	g := &gateway{domains: []string{"builds.example.com"}, namespace: "ns", port: 1234, dialTO: 300 * time.Millisecond}
	runHandle(t, g, func(peer net.Conn) {
		_ = tls.Client(peer, &tls.Config{
			ServerName:         "buildkitd-p1a2b3c4d5e6f7a8.builds.example.com",
			InsecureSkipVerify: true,
		}).Handshake()
	})
}
