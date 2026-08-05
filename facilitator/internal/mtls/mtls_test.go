package mtls_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/zerkerlabs/gateway/facilitator/internal/mtls"
	"github.com/zerkerlabs/gateway/facilitator/internal/mtls/mtlstest"
)

// startTestServer starts an HTTPS listener enforcing mTLS via
// mtls.ServerConfig and returns its address. The handler always responds 200
// OK so tests can distinguish "handshake rejected" (no HTTP response at all)
// from "request reached the application".
func startTestServer(t *testing.T, serverCert tls.Certificate, clientCAs *x509.CertPool) string {
	t.Helper()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", mtls.ServerConfig(serverCert, clientCAs))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String()
}

func TestServerConfig_HandshakeRequiresClientCert(t *testing.T) {
	t.Parallel()

	ca := mtlstest.NewCA(t)
	serverIssued := ca.IssueLeaf(t, "facilitator-under-test", x509.ExtKeyUsageServerAuth)
	clientIssued := ca.IssueLeaf(t, "trusted-client", x509.ExtKeyUsageClientAuth)
	otherCA := mtlstest.NewCA(t)
	untrustedClient := otherCA.IssueLeaf(t, "untrusted-client", x509.ExtKeyUsageClientAuth)

	clientCAs, err := mtls.ClientCAPool(ca.CertPEM)
	if err != nil {
		t.Fatalf("ClientCAPool: %v", err)
	}
	addr := startTestServer(t, serverIssued.TLSCert, clientCAs)

	rootPool := x509.NewCertPool()
	rootPool.AddCert(ca.Cert)

	tests := []struct {
		name       string
		clientCert []tls.Certificate
		wantErr    bool
	}{
		{
			name:       "no client certificate",
			clientCert: nil,
			wantErr:    true,
		},
		{
			name:       "client certificate signed by a different CA",
			clientCert: []tls.Certificate{untrustedClient.TLSCert},
			wantErr:    true,
		},
		{
			name:       "valid client certificate",
			clientCert: []tls.Certificate{clientIssued.TLSCert},
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						RootCAs:      rootPool,
						Certificates: tt.clientCert,
					},
				},
			}

			resp, err := client.Get("https://" + addr + "/")
			if tt.wantErr {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatalf("expected handshake to fail, got a response")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected handshake to succeed, got: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
		})
	}
}

func TestClientCAPool_InvalidPEM(t *testing.T) {
	t.Parallel()

	if _, err := mtls.ClientCAPool([]byte("not a pem certificate")); err == nil {
		t.Fatalf("expected an error for invalid PEM input")
	}
}
