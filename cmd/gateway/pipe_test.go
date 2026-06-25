package main

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pipe must forward bytes in BOTH directions and complete once both ends close. We wire two TCP
// socket pairs (client<->a, b<->backend), run pipe(a,b), and check payloads cross each way.
func TestPipe_BidirectionalForwarding(t *testing.T) {
	clientConn, a := tcpPair(t)
	b, backendConn := tcpPair(t)

	done := make(chan struct{})
	go func() { pipe(a, b); close(done) }()

	// client -> backend
	if _, err := clientConn.Write([]byte("hello-up")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, backendConn, 8); got != "hello-up" {
		t.Fatalf("backend got %q, want hello-up", got)
	}
	// backend -> client
	if _, err := backendConn.Write([]byte("hello-dn")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, clientConn, 8); got != "hello-dn" {
		t.Fatalf("client got %q, want hello-dn", got)
	}

	// Closing both outer ends must let pipe() return (both io.Copy reach EOF).
	_ = clientConn.Close()
	_ = backendConn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not return after both ends closed")
	}
}

// A half-close on one direction (client done sending) must NOT truncate the other direction: the
// backend can still stream a long response back. This is the property a naive "close both on first
// EOF" breaks.
func TestPipe_HalfCloseKeepsReverseOpen(t *testing.T) {
	clientConn, a := tcpPair(t)
	b, backendConn := tcpPair(t)

	done := make(chan struct{})
	go func() { pipe(a, b); close(done) }()

	// Client signals EOF upstream (CloseWrite), but keeps reading.
	if err := clientConn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// Backend should still receive the upstream EOF and be able to write a response afterwards.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Drain until EOF so the backend sees the half-close propagated.
		_, _ = io.Copy(io.Discard, backendConn)
	}()
	if _, err := backendConn.Write([]byte("late-response")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, clientConn, len("late-response")); got != "late-response" {
		t.Fatalf("client got %q after half-close, want late-response", got)
	}

	_ = clientConn.Close()
	_ = backendConn.Close()
	wg.Wait()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe did not return")
	}
}

// tcpPair returns the two ends of a connected TCP socket pair.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted := <-ch
	if accepted.err != nil {
		t.Fatal(accepted.err)
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.c.Close() })
	return dialed, accepted.c
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("readN: %v", err)
	}
	return string(buf)
}
