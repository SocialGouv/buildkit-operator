// Command gateway is buildkit-operator's single shared SNI router for off-cluster CI. It terminates NO TLS:
// it peeks the TLS ClientHello's SNI (<daemon>.<domain>), then pipes the still-encrypted connection
// straight to that project's daemon ClusterIP Service (<daemon>.<ns>.svc:<port>) — mTLS stays
// end-to-end to the daemon (client-cert auth intact). One LoadBalancer fronts every daemon, instead
// of one public LB per daemon (which doesn't scale with project count).
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type gateway struct {
	domain    string
	namespace string
	port      int
	dialTO    time.Duration
	maxConns  chan struct{} // bounds concurrent connections (nil = unlimited)
	inflight  sync.WaitGroup
}

func main() {
	g := &gateway{dialTO: 10 * time.Second}
	var listen, healthListen string
	var maxConns int
	flag.StringVar(&listen, "listen", ":1234", "TCP listen address")
	flag.StringVar(&healthListen, "health-listen", ":8081", "HTTP address for /healthz and /readyz")
	flag.StringVar(&g.domain, "domain", os.Getenv("BUILDKIT_OPERATOR_GATEWAY_DOMAIN"), "gateway domain; the SNI is <daemon>.<domain> (required)")
	flag.StringVar(&g.namespace, "namespace", envOr("BUILDKIT_OPERATOR_NAMESPACE", "buildkit-operator"), "namespace the daemons run in")
	flag.IntVar(&g.port, "daemon-port", 1234, "daemon mTLS port")
	flag.IntVar(&maxConns, "max-conns", 0, "max concurrent connections (0 = unlimited; bounds resource use under abuse)")
	flag.Parse()
	if g.domain == "" {
		slog.Error("--domain (or BUILDKIT_OPERATOR_GATEWAY_DOMAIN) is required")
		os.Exit(2)
	}
	if maxConns > 0 {
		g.maxConns = make(chan struct{}, maxConns)
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}

	// Graceful shutdown: stop accepting on SIGTERM/SIGINT, then let in-flight builds drain (bounded).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received, closing listener")
		_ = ln.Close()
	}()

	go g.serveHealth(ctx, healthListen)

	slog.Info("buildkit-operator gateway listening", "addr", listen, "domain", g.domain, "namespace", g.namespace)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // listener closed by shutdown
			}
			// Transient accept error (e.g. fd pressure): back off briefly instead of busy-spinning.
			slog.Warn("accept", "err", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		go g.handle(c)
	}

	// Drain in-flight connections with a bounded grace, then exit.
	done := make(chan struct{})
	go func() { g.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(115 * time.Second): // under the pod terminationGracePeriod
		slog.Warn("drain timeout, exiting with connections still open")
	}
	slog.Info("gateway stopped")
}

// serveHealth runs the liveness/readiness endpoint so the kubelet can detect a wedged gateway.
func (g *gateway) serveHealth(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("health server", "err", err)
	}
}

// handle peeks the SNI, resolves the daemon backend, and pipes the (still-encrypted) connection.
func (g *gateway) handle(client net.Conn) {
	defer client.Close()
	if g.maxConns != nil {
		select {
		case g.maxConns <- struct{}{}:
			defer func() { <-g.maxConns }()
		default:
			slog.Warn("rejecting connection: max-conns reached", "remote", client.RemoteAddr().String())
			return
		}
	}
	g.inflight.Add(1)
	defer g.inflight.Done()
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
// anything outside the gateway domain or that is not a buildkit-operator daemon name (defense in depth — a
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

// pipe copies bytes both ways and waits for BOTH directions to finish. On EOF in one direction it
// half-closes the write side of the peer (CloseWrite) so the other direction can still drain — a plain
// "close both on first EOF" truncates long build streams.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite() // signal EOF to dst, let the reverse direction finish
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
