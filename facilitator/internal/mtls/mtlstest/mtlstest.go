// Package mtlstest generates throwaway CA and leaf certificates for tests
// exercising the facilitator's mTLS termination and account mapping (spec
// 0007 T1). None of this material is suitable for anything but a test
// process — private keys are generated fresh per call and never persisted.
package mtlstest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// serialNumberLimit bounds the random certificate serial numbers this
// package mints (128 bits, matching common x509 practice).
var serialNumberLimit = new(big.Int).Lsh(big.NewInt(1), 128)

// CA is a throwaway certificate authority for tests.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
}

// NewCA generates a throwaway self-signed CA certificate and key.
func NewCA(t testing.TB) *CA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mtlstest: generate CA key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatalf("mtlstest: generate CA serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "facilitator test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mtlstest: create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("mtlstest: parse CA certificate: %v", err)
	}

	return &CA{
		Cert:    cert,
		CertPEM: pemEncodeCert(der),
		key:     key,
	}
}

// IssuedCert bundles an issued leaf certificate with its parsed form and a
// ready-to-use tls.Certificate (for presenting as a client or server cert).
type IssuedCert struct {
	Cert    *x509.Certificate
	TLSCert tls.Certificate
}

// IssueLeaf issues a leaf certificate for commonName signed by ca, with the
// given extended key usages (e.g. x509.ExtKeyUsageClientAuth).
func (ca *CA) IssueLeaf(t testing.TB, commonName string, extKeyUsage ...x509.ExtKeyUsage) IssuedCert {
	t.Helper()
	return issueLeaf(t, ca.Cert, ca.key, commonName, extKeyUsage...)
}

// SelfSigned issues a leaf certificate for commonName that is self-signed —
// i.e. not chained to any CA. Use it to exercise the "certificate not signed
// by our client CA" handshake-rejection case.
func SelfSigned(t testing.TB, commonName string, extKeyUsage ...x509.ExtKeyUsage) IssuedCert {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mtlstest: generate self-signed key: %v", err)
	}
	tmpl := leafTemplate(t, commonName, extKeyUsage...)

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mtlstest: create self-signed certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("mtlstest: parse self-signed certificate: %v", err)
	}

	return IssuedCert{
		Cert: cert,
		TLSCert: tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        cert,
		},
	}
}

func issueLeaf(t testing.TB, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, extKeyUsage ...x509.ExtKeyUsage) IssuedCert {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mtlstest: generate leaf key: %v", err)
	}
	tmpl := leafTemplate(t, commonName, extKeyUsage...)

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("mtlstest: create leaf certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("mtlstest: parse leaf certificate: %v", err)
	}

	return IssuedCert{
		Cert: cert,
		TLSCert: tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        cert,
		},
	}
}

func leafTemplate(t testing.TB, commonName string, extKeyUsage ...x509.ExtKeyUsage) *x509.Certificate {
	t.Helper()

	serial, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatalf("mtlstest: generate leaf serial: %v", err)
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  extKeyUsage,
		// SANs cover both loopback forms so a leaf can double as a server cert
		// for tests dialing 127.0.0.1 or localhost; irrelevant for client-auth
		// leaves, which are verified against the client CA pool, not a hostname.
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:    []string{"localhost"},
	}
}

func pemEncodeCert(der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return buf.Bytes()
}
