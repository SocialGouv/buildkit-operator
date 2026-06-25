package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"time"

	"github.com/go-logr/logr"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// warnIfDaemonCertMissingGatewaySAN cross-checks the daemon server cert against the gateway domain at
// boot. Off-cluster CI dials <daemon>.<gateway-host>, so the cert MUST carry a "*.<gateway-host>" SAN
// or every off-cluster build fails the TLS handshake with an opaque cert error. We surface that as a
// loud startup warning instead. Non-fatal and best-effort: any lookup/parse failure is just logged,
// since in-cluster routing doesn't depend on this and we must not block buildd from starting.
func warnIfDaemonCertMissingGatewaySAN(restCfg *rest.Config, cfg builder.Config, gatewayHost string, log logr.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := client.New(restCfg, client.Options{})
	if err != nil {
		log.Info("gateway SAN check skipped: cannot build client", "err", err.Error())
		return
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: cfg.DaemonCertsSecret, Namespace: cfg.Namespace}, &sec); err != nil {
		log.Info("gateway SAN check skipped: cannot read daemon certs Secret", "secret", cfg.DaemonCertsSecret, "err", err.Error())
		return
	}
	// mkcert path stores cert.pem; cert-manager stores tls.crt — accept either.
	raw := sec.Data["cert.pem"]
	if len(raw) == 0 {
		raw = sec.Data["tls.crt"]
	}
	if len(raw) == 0 {
		log.Info("gateway SAN check skipped: daemon certs Secret has no cert.pem/tls.crt", "secret", cfg.DaemonCertsSecret)
		return
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		log.Info("gateway SAN check skipped: cert is not PEM", "secret", cfg.DaemonCertsSecret)
		return
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Info("gateway SAN check skipped: cannot parse cert", "err", err.Error())
		return
	}
	want := "*." + gatewayHost
	for _, dns := range crt.DNSNames {
		if dns == want || dns == gatewayHost {
			return // covered
		}
	}
	log.Info("WARNING: daemon cert has no SAN covering the gateway domain — off-cluster builds will fail TLS; regenerate certs (GATEWAY_HOST=<host> deploy/cert/create-certs.sh, or set gateway.host before issuing)",
		"gateway_host", gatewayHost, "want_san", want, "cert_sans", crt.DNSNames)
}
