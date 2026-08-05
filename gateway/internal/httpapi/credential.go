package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/zerkerlabs/gateway/gateway/internal/auth"
	"github.com/zerkerlabs/gateway/gateway/internal/credential"
)

// CredentialService is the interface the credential HTTP handlers require.
// *credential.Service satisfies it; exporting it lets callers (e.g. the server
// Config) depend on the behaviour rather than the concrete implementation.
type CredentialService interface {
	Create(ctx context.Context, tenantID string, p credential.CreateParams) (*credential.Credential, error)
	Get(ctx context.Context, tenantID, id string) (*credential.Credential, error)
	List(ctx context.Context, tenantID string) ([]*credential.Credential, error)
	Update(ctx context.Context, tenantID, id string, p credential.UpdateParams) (*credential.Credential, error)
	Delete(ctx context.Context, tenantID, id string) error
}

// createCredentialRequest carries the inputs for POST /v1/credentials.
// Plaintext is sensitive (invariant #4, AGENTS.md): it must never be logged,
// returned in a response body, or stored in plaintext form.
type createCredentialRequest struct {
	Name      string `json:"name"`
	AuthType  string `json:"auth_type"`
	Source    string `json:"source"`
	Plaintext string `json:"plaintext"` // source=managed; envelope-encrypted before any persistence
	VaultRef  string `json:"vault_ref"` // source=vault; opaque path in external secrets store
}

// putCredentialRequest carries the fields for PUT /v1/credentials/{id}.
// All fields are optional; nil means leave unchanged.
// Plaintext is sensitive (invariant #4): same restrictions as createCredentialRequest.
type putCredentialRequest struct {
	Name      *string `json:"name"`
	AuthType  *string `json:"auth_type"`
	Plaintext *string `json:"plaintext"` // source=managed; re-encrypts under current KEK when non-nil
	VaultRef  *string `json:"vault_ref"` // source=vault
}

// credentialResponse is the JSON shape returned for every credential read.
// Ciphertext fields (encrypted_dek, encrypted_secret, kek_version) are never
// present — only metadata and the masked_hint are exposed (invariant #4).
type credentialResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AuthType   string    `json:"auth_type"`
	Source     string    `json:"source"`
	MaskedHint string    `json:"masked_hint,omitempty"` // non-empty for source=managed only
	VaultRef   string    `json:"vault_ref,omitempty"`   // non-empty for source=vault only
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type credentialListResponse struct {
	Credentials []credentialResponse `json:"credentials"`
}

func toCredentialResponse(c *credential.Credential) credentialResponse {
	return credentialResponse{
		ID:         c.ID,
		Name:       c.Name,
		AuthType:   string(c.AuthType),
		Source:     string(c.Source),
		MaskedHint: c.MaskedHint,
		VaultRef:   c.VaultRef,
		Version:    c.Version,
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
	}
}

// validAuthType returns true when s is one of the recognised auth_type values.
func validAuthType(s string) bool {
	switch credential.AuthType(s) {
	case credential.AuthTypeBearer, credential.AuthTypeAPIKey, credential.AuthTypeNone:
		return true
	}
	return false
}

func (h *Handler) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var req createCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validAuthType(req.AuthType) {
		writeError(w, http.StatusBadRequest, "auth_type must be bearer, api_key, or none")
		return
	}

	src := credential.Source(req.Source)
	switch src {
	case credential.SourceManaged:
		if req.Plaintext == "" {
			writeError(w, http.StatusBadRequest, "plaintext is required for managed credentials")
			return
		}
	case credential.SourceVault:
		if req.VaultRef == "" {
			writeError(w, http.StatusBadRequest, "vault_ref is required for vault credentials")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "source must be managed or vault")
		return
	}

	params := credential.CreateParams{
		Name:     req.Name,
		AuthType: credential.AuthType(req.AuthType),
		Source:   src,
		VaultRef: req.VaultRef,
	}
	// Plaintext only applies to managed credentials; the service ignores it for
	// vault, but don't hand it a value it shouldn't have.
	if src == credential.SourceManaged {
		params.Plaintext = []byte(req.Plaintext)
	}

	c, err := h.credSvc.Create(r.Context(), tenant, params)
	if err != nil {
		if errors.Is(err, credential.ErrNameConflict) {
			writeError(w, http.StatusConflict, "credential name already exists in this tenant")
			return
		}
		h.logger.Error("create credential: service error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, toCredentialResponse(c))
}

func (h *Handler) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	// The middleware sets tenant and user from the same validated token, so
	// checking either alone is sufficient; checking both is the project
	// convention (applied uniformly across all credential handlers).
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	creds, err := h.credSvc.List(r.Context(), tenant)
	if err != nil {
		h.logger.Error("list credentials: service error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp := credentialListResponse{
		Credentials: make([]credentialResponse, len(creds)),
	}
	for i, c := range creds {
		resp.Credentials[i] = toCredentialResponse(c)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")
	c, err := h.credSvc.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		h.logger.Error("get credential: service error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toCredentialResponse(c))
}

func (h *Handler) handlePutCredential(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")

	var req putCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name != nil && *req.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	if req.AuthType != nil && !validAuthType(*req.AuthType) {
		writeError(w, http.StatusBadRequest, "auth_type must be bearer, api_key, or none")
		return
	}
	// A present-but-empty plaintext must be rejected: the service treats a
	// non-nil (even zero-length) value as "rotate to this", so an empty string
	// would overwrite the secret with the encryption of zero bytes and silently
	// disable upstream auth. nil (key absent) still means "leave unchanged".
	if req.Plaintext != nil && *req.Plaintext == "" {
		writeError(w, http.StatusBadRequest, "plaintext must not be empty")
		return
	}

	params := credential.UpdateParams{
		Name:     req.Name,
		VaultRef: req.VaultRef,
	}
	if req.AuthType != nil {
		at := credential.AuthType(*req.AuthType)
		params.AuthType = &at
	}
	if req.Plaintext != nil {
		params.Plaintext = []byte(*req.Plaintext)
	}

	c, err := h.credSvc.Update(r.Context(), tenant, id, params)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, credential.ErrNameConflict) {
			writeError(w, http.StatusConflict, "credential name already exists in this tenant")
			return
		}
		h.logger.Error("update credential: service error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, toCredentialResponse(c))
}

func (h *Handler) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	tenant := auth.TenantFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	if tenant == "" || user == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	id := r.PathValue("id")
	if err := h.credSvc.Delete(r.Context(), tenant, id); err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, credential.ErrReferenced) {
			writeError(w, http.StatusConflict, "credential is referenced by one or more agents")
			return
		}
		h.logger.Error("delete credential: service error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
