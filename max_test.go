package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPublicMediaDialReportsWhenNoPublicAddressExists(t *testing.T) {
	conn, err := dialPublicMedia(t.Context(), "tcp", "127.0.0.1:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("private address must not be dialed")
	}
	if err == nil || !strings.Contains(err.Error(), "нет публичного адреса") {
		t.Fatalf("missing public-address diagnosis: %v", err)
	}
}

func TestPublicMediaDialPreservesCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn, err := dialPublicMedia(ctx, "tcp", "1.1.1.1:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("cancelled dial must not connect")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial cancellation cause lost: %v", err)
	}
}

func TestEmbeddedRootVerifiesTheMaxChain(t *testing.T) {
	transport, err := maxTransport()
	if err != nil {
		t.Fatal(err)
	}

	// The intermediate MAX sends with its certificate must chain up to the embedded root
	data, err := os.ReadFile("testdata/russian_trusted_sub_ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	sub, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Verify(x509.VerifyOptions{Roots: transport.TLSClientConfig.RootCAs}); err != nil {
		t.Fatalf("sub CA does not verify against the embedded root: %v", err)
	}
}
