// Package mtls builds the server-side TLS configuration the facilitator uses
// to terminate mutual TLS (spec 0007 Decision 3). It only handles transport:
// requiring and verifying a client certificate against a configured client
// CA. Mapping the verified certificate to a facilitator account is the
// account package's concern.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// ClientCAPool parses one or more PEM-encoded CA certificates and returns a
// pool suitable for tls.Config.ClientCAs. Returns an error if pemCerts
// contains no valid certificate.
func ClientCAPool(pemCerts []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemCerts) {
		return nil, fmt.Errorf("mtls: no valid certificates found in client CA PEM")
	}
	return pool, nil
}

// ServerConfig returns the *tls.Config the facilitator's HTTP server uses to
// terminate mutual TLS: it presents serverCert to the caller and requires +
// verifies the caller's certificate against clientCAs. A connection
// presenting no certificate, or one that does not chain to clientCAs, fails
// at the handshake — the request never reaches application code (spec 0007:
// "no valid cert ⇒ handshake rejected").
func ServerConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
}
