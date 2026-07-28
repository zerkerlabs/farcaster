package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
	"github.com/zerkerlabs/farcaster/rooms/internal/tenant"
)

// postMessageRequest is the request body for POST /v1/rooms/{rom_id}/messages.
// ToMemberID is optional: when set, the message is addressed to another
// member and is delivered as a proxied call to that member's agent through
// the gateway (rooms/internal/gateway) before being recorded as a room
// message. When empty, the message is recorded directly, exactly as before.
type postMessageRequest struct {
	MemberID   string `json:"member_id"`
	ToMemberID string `json:"to_member_id"`
	Body       string `json:"body"`
}

func (h *Handler) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.FromContext(r.Context())
	if tenantID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	roomID := r.PathValue("rom_id")

	var req postMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.MemberID == "" {
		writeError(w, http.StatusBadRequest, "member_id is required")
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}

	// An addressed message is delivered as a proxied call to the recipient's
	// agent before it is ever recorded, so a failed call can never look like
	// a delivered message.
	//
	// But delivery only happens once the request is known to be valid. The
	// gateway call is a REAL side effect on another member's agent, with
	// policy and payment consequences; firing it for a terminated room, an
	// out-of-turns room, or a spoofed sender — and only then having
	// AppendMessage reject the request — would mean a live call that the room
	// has no record of at all.
	//
	// On a failed pre-check, fall through with no delivery: AppendMessage
	// below produces the authoritative error and any state transition it
	// implies (exceeding the budget abandons the room), so the outcome is
	// identical to an unaddressed message minus the call.
	if req.ToMemberID != "" && h.store.CheckCanPost(r.Context(), tenantID, roomID, req.MemberID) == nil {
		if !h.deliverToMember(w, r, tenantID, roomID, req) {
			return
		}
	}

	msg, err := h.store.AppendMessage(r.Context(), tenantID, roomID, req.MemberID, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, room.ErrNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, room.ErrRoomTerminated):
			writeError(w, http.StatusConflict, "room is terminated")
		case errors.Is(err, room.ErrTurnBudgetExceeded):
			writeError(w, http.StatusConflict, "room turn budget exceeded")
		case errors.Is(err, room.ErrMemberNotFound):
			writeError(w, http.StatusBadRequest, "member not found in room")
		default:
			h.logger.Error("post message: store error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusCreated, toMessageResponse(msg))
}

// deliveredMessage is the payload a proxied call carries to the recipient
// agent. Rooms does not interpret or transform the body — the gateway and the
// recipient agent are what act on it — but it does carry the room and sender
// alongside it so the recipient knows which conversation the message belongs
// to, and so a delivery can be correlated with the room's event log.
type deliveredMessage struct {
	RoomID       string `json:"room_id"`
	FromMemberID string `json:"from_member_id"`
	ToMemberID   string `json:"to_member_id"`
	Body         string `json:"body"`
}

// deliverToMember resolves req.ToMemberID to its agent and delivers req.Body
// as a single proxied call through the gateway. It writes an error response
// and returns false on any failure: an unknown room/recipient, a call the
// gateway rejected or that failed at the recipient, or one whose outcome could
// not be confirmed — each recorded as an EventDeliveryFailed event carrying
// its class, so a failed or unconfirmed call is never indistinguishable from a
// delivered one.
//
// Returns true only once delivery is CONFIRMED, at which point the caller
// proceeds to record the message itself. The gateway's proxy is asynchronous,
// so the client polls the invocation to completion rather than trusting the
// 202 that accepts it.
//
// Callers must establish that the message would be accepted (see
// Store.CheckCanPost) before calling this — it performs a real side effect on
// another member's agent.
func (h *Handler) deliverToMember(w http.ResponseWriter, r *http.Request, tenantID, roomID string, req postMessageRequest) bool {
	toAgentID, err := h.store.MemberAgentID(r.Context(), tenantID, roomID, req.ToMemberID)
	if err != nil {
		switch {
		case errors.Is(err, room.ErrNotFound):
			writeError(w, http.StatusNotFound, "room not found")
		case errors.Is(err, room.ErrMemberNotFound):
			writeError(w, http.StatusBadRequest, "to_member_id not found in room")
		default:
			h.logger.Error("post message: resolve recipient", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return false
	}

	payload, err := json.Marshal(deliveredMessage{
		RoomID:       roomID,
		FromMemberID: req.MemberID,
		ToMemberID:   req.ToMemberID,
		Body:         req.Body,
	})
	if err != nil {
		h.logger.Error("post message: marshal delivered payload", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}

	result, callErr := h.gateway.Call(r.Context(), toAgentID, payload)
	if callErr == nil {
		// Confirmed delivered. Record the gateway's invocation ID so this
		// room's transcript can be reconciled against the gateway's own
		// invocation record when debugging.
		if err := h.store.RecordDelivery(r.Context(), tenantID, roomID, req.MemberID, req.ToMemberID, toAgentID, result.InvocationID); err != nil {
			h.logger.Error("post message: record delivery", "err", err)
		}
		return true
	}

	class := gateway.ErrorClassUpstreamFailure
	var ce *gateway.CallError
	if errors.As(callErr, &ce) {
		class = ce.Class
	}
	if err := h.store.RecordDeliveryFailure(r.Context(), tenantID, roomID, req.MemberID, req.ToMemberID, toAgentID, string(class)); err != nil {
		h.logger.Error("post message: record delivery failure", "err", err)
	}

	// The gateway's response is classified, never forwarded: a non-2xx body
	// must never leak into the room transcript or this API response
	// (AGENTS.md invariant #3).
	switch class {
	case gateway.ErrorClassCallerError:
		writeError(w, http.StatusUnprocessableEntity, "message could not be delivered: the gateway rejected the call")
	case gateway.ErrorClassUnconfirmed:
		// The call was accepted but its outcome is unknown. Reported
		// distinctly from a failure: it may have reached the recipient, so the
		// caller must not assume it can safely retry as if nothing happened.
		writeError(w, http.StatusGatewayTimeout, "message delivery could not be confirmed: the call was accepted but did not complete in time")
	default:
		writeError(w, http.StatusBadGateway, "message could not be delivered: gateway upstream failure")
	}
	return false
}
