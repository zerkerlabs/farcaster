package account_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkerlabs/farcaster/facilitator/internal/account"
	"github.com/zerkerlabs/farcaster/facilitator/internal/mtls/mtlstest"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()

	ca := mtlstest.NewCA(t)
	activeCert := ca.IssueLeaf(t, "active-client", x509.ExtKeyUsageClientAuth)
	inactiveCert := ca.IssueLeaf(t, "inactive-client", x509.ExtKeyUsageClientAuth)
	unknownCert := ca.IssueLeaf(t, "unknown-client", x509.ExtKeyUsageClientAuth)

	store := account.NewMemoryStore()
	activeAccount := &account.Account{
		Name:            "active-operator",
		CertFingerprint: account.Fingerprint(activeCert.Cert),
		Active:          true,
	}
	if err := store.Create(context.Background(), activeAccount); err != nil {
		t.Fatalf("Create(active): %v", err)
	}
	if err := store.Create(context.Background(), &account.Account{
		Name:            "inactive-operator",
		CertFingerprint: account.Fingerprint(inactiveCert.Cert),
		Active:          false,
	}); err != nil {
		t.Fatalf("Create(inactive): %v", err)
	}

	tests := []struct {
		name       string
		peerCert   *x509.Certificate // nil means no TLS at all
		noTLS      bool
		wantStatus int
		wantNext   bool
	}{
		{
			name:       "no TLS connection state",
			noTLS:      true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unknown certificate fingerprint",
			peerCert:   unknownCert.Cert,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "known but inactive account",
			peerCert:   inactiveCert.Cert,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "known and active account",
			peerCert:   activeCert.Cert,
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reachedNext bool
			var gotAccount *account.Account
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reachedNext = true
				gotAccount, _ = account.FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			handler := account.Middleware(store, logger)(next)

			req := httptest.NewRequest(http.MethodPost, "/settle", nil)
			if !tt.noTLS {
				req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{tt.peerCert}}
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if reachedNext != tt.wantNext {
				t.Fatalf("reached next handler = %v, want %v", reachedNext, tt.wantNext)
			}
			if tt.wantNext {
				if gotAccount == nil || gotAccount.Name != activeAccount.Name {
					t.Fatalf("FromContext in handler = %+v, want the active account", gotAccount)
				}
			}
			if !tt.wantNext && rec.Code == http.StatusForbidden {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				if body["error"] != "unknown facilitator account" {
					t.Fatalf("error body = %q, want %q", body["error"], "unknown facilitator account")
				}
			}
		})
	}
}
