// Command gateway is buildcat's single shared SNI router for off-cluster CI. It terminates NO TLS:
// it peeks the TLS ClientHello's SNI (<daemon>.<domain>), then pipes the still-encrypted connection
// straight to that project's daemon ClusterIP Service (<daemon>.<ns>.svc:<port>) — mTLS stays
// end-to-end to the daemon (client-cert auth intact). One LoadBalancer fronts every daemon, instead
// of one public LB per daemon (which doesn't scale with project count).
package main

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type gateway struct {
	domain    string
	namespace string
	port      int
	dialTO    time.Duration
}

func main() {
	g := &gateway{dialTO: 10 * time.Second}
	var listen string
	flag.StringVar(&listen, "listen", ":1234", "TCP listen address")
	flag.StringVar(&g.domain, "domain", os.Getenv("BUILDCAT_GATEWAY_DOMAIN"), "gateway domain; the SNI is <daemon>.<domain> (required)")
	flag.StringVar(&g.namespace, "namespace", envOr("BUILDCAT_NAMESPACE", "buildcat"), "namespace the daemons run in")
	flag.IntVar(&g.port, "daemon-port", 1234, "daemon mTLS port")
	flag.Parse()
	if g.domain == "" {
		slog.Error("--domain (or BUILDCAT_GATEWAY_DOMAIN) is required")
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	slog.Info("buildcat gateway listening", "addr", listen, "domain", g.domain, "namespace", g.namespace)
	for {
		c, err := ln.Accept()
		if err != nil {
			slog.Error("accept", "err", err)
			continue
		}
		go g.handle(c)
	}
}

// handle peeks the SNI, resolves the daemon backend, and pipes the (still-encrypted) connection.
func (g *gateway) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	sni, raw, err := peekClientHelloSNI(client)
	if err != nil {
		slog.Warn("peek SNI", "err", err)
		return
	}
	backend, err := g.backendFor(sni)
	if err != nil {
		slog.Warn("reject", "sni", sni, "err", err)
		return
	}
	_ = client.SetReadDeadline(time.Time{}) // clear the handshake deadline for the build itself

	upstream, err := net.DialTimeout("tcp", backend, g.dialTO)
	if err != nil {
		slog.Warn("dial backend", "backend", backend, "err", err)
		return
	}
	defer upstream.Close()
	if _, err := upstream.Write(raw); err != nil { // replay the buffered ClientHello
		return
	}
	slog.Info("routed", "sni", sni, "backend", backend)
	pipe(client, upstream)
}

// backendFor maps an SNI <daemon>.<domain> to the daemon's in-cluster Service address. It rejects
// anything outside the gateway domain or that is not a buildcat daemon name (defense in depth — a
// caller can't be routed to an arbitrary host or another namespace).
func (g *gateway) backendFor(sni string) (string, error) {
	suffix := "." + g.domain
	if !strings.HasSuffix(sni, suffix) {
		return "", errors.New("SNI outside gateway domain")
	}
	name := strings.TrimSuffix(sni, suffix)
	if !strings.HasPrefix(name, "buildkitd-") || strings.ContainsAny(name, "./") {
		return "", errors.New("SNI is not a daemon name")
	}
	return name + "." + g.namespace + ".svc:" + strconv.Itoa(g.port), nil
}

// pipe copies bytes both ways until either side closes (the deferred Close unblocks the other copy).
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { _, _ = io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
