// Package httpapi implements the Rooms v1 HTTP API: the five routes that
// create a room, read it back, seat a member, post a message, and read the
// room's receipts. Each handler lives in its own file, following the shape
// established in the gateway module's httpapi package.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/memory"
	"github.com/zerkerlabs/farcaster/rooms/internal/receipt"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
)

// GatewayCaller is the subset of *gateway.Client the message handlers need:
// delivering one proxied call to a member's agent through the Farcaster
// gateway and confirming it actually completed. *gateway.Client satisfies this
// interface.
//
// Call returns only once delivery is confirmed — the gateway's proxy is
// asynchronous, so an accepted call is not yet a delivered one. It takes the
// tenant the call is made on behalf of: the gateway attributes a proxied call
// to whichever tenant the credential belongs to, so the tenant has to be
// carried explicitly rather than assumed.
type GatewayCaller interface {
	Call(ctx context.Context, tenantID, agentID string, body []byte) (*gateway.Result, error)
}

// Handler holds the shared dependencies for the Rooms HTTP handlers.
type Handler struct {
	store       *room.Store
	memoryStore memory.Store
	gateway     GatewayCaller
	emitter     receipt.Emitter
	logger      *slog.Logger

	// emitWG tracks in-flight receipt-emission goroutines so Shutdown can
	// drain them before the process exits — an emitter goroutine must not
	// outlive the service.
	emitWG sync.WaitGroup
	// shutdownCtx is cancelled by Shutdown so an in-flight emission aborts
	// promptly instead of running out its own timeout.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// NewHandler returns a Handler backed by store, logging to logger. memoryStore
// is the seam onboarding a member reads from (rooms/internal/memory).
// gatewayClient delivers a message addressed to another member as a proxied
// call to that member's agent (rooms/internal/gateway) — every agent-to-agent
// call goes through it, never direct. emitter records a trust receipt for
// each such call, asynchronously and fail-open (rooms/internal/receipt).
func NewHandler(store *room.Store, memoryStore memory.Store, gatewayClient GatewayCaller, emitter receipt.Emitter, logger *slog.Logger) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{
		store:          store,
		memoryStore:    memoryStore,
		gateway:        gatewayClient,
		emitter:        emitter,
		logger:         logger,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
}

// Shutdown cancels any in-flight receipt emission and waits for its
// goroutines to finish, so none outlives the service. ctx bounds the wait;
// if its deadline expires before every goroutine has drained, Shutdown
// returns ctx.Err().
func (h *Handler) Shutdown(ctx context.Context) error {
	h.shutdownCancel()

	done := make(chan struct{})
	go func() {
		h.emitWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RegisterRoutes mounts the five v1 room routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rooms", h.handleCreateRoom)
	mux.HandleFunc("GET /v1/rooms/{rom_id}", h.handleGetRoom)
	mux.HandleFunc("POST /v1/rooms/{rom_id}/members", h.handleAddMember)
	mux.HandleFunc("POST /v1/rooms/{rom_id}/messages", h.handlePostMessage)
	mux.HandleFunc("GET /v1/rooms/{rom_id}/receipts", h.handleGetReceipts)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
