package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// selfSignedPEM issues a throwaway cert carrying the given DNS SANs, PEM-encoded.
func selfSignedPEM(t *testing.T, dnsNames ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCertCoversGateway(t *testing.T) {
	host := "bko.example.com"

	t.Run("wildcard SAN covers", func(t *testing.T) {
		covered, _, err := certCoversGateway(selfSignedPEM(t, "*."+host), host)
		if err != nil || !covered {
			t.Errorf("wildcard: covered=%v err=%v, want true/nil", covered, err)
		}
	})

	t.Run("bare host SAN covers", func(t *testing.T) {
		covered, _, err := certCoversGateway(selfSignedPEM(t, host), host)
		if err != nil || !covered {
			t.Errorf("bare host: covered=%v err=%v, want true/nil", covered, err)
		}
	})

	t.Run("unrelated SAN not covered", func(t *testing.T) {
		covered, sans, err := certCoversGateway(selfSignedPEM(t, "other.example.com"), host)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if covered {
			t.Error("unrelated SAN should not cover the gateway domain")
		}
		if len(sans) != 1 || sans[0] != "other.example.com" {
			t.Errorf("sans = %v, want [other.example.com]", sans)
		}
	})

	t.Run("non-PEM input errors", func(t *testing.T) {
		if _, _, err := certCoversGateway([]byte("not a cert"), host); err == nil {
			t.Error("want error on non-PEM input")
		}
	})

	t.Run("garbage DER errors", func(t *testing.T) {
		bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})
		if _, _, err := certCoversGateway(bad, host); err == nil {
			t.Error("want error on unparseable cert")
		}
	})
}
