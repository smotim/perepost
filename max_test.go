package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
)

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
