package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zerkerlabs/farcaster/rooms/internal/auth"
	"github.com/zerkerlabs/farcaster/rooms/internal/gateway"
	"github.com/zerkerlabs/farcaster/rooms/internal/room"
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
	tenantID := auth.TenantFromContext(r.Context())
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
	// agent before it is ever recorded, so a failed call can never look like a
	// delivered message. That call is a REAL side effect on another member's
	// agent, with policy and payment consequences, so it happens under a turn
	// reservation — see postAddressedMessage.
	//
	// The two paths are exclusive on purpose. An addressed message must never
	// be able to reach the plain append below: that path takes no ToMemberID
	// and makes no call, so an addressed request arriving there would be
	// silently recorded as a broadcast, with a 201 and no proxied call at all
	// — the bypass this whole package exists to prevent. Every way the
	// addressed path can fail is answered inside it.
	if req.ToMemberID != "" {
		h.postAddressedMessage(w, r, tenantID, roomID, req)
		return
	}

	msg, err := h.store.AppendMessage(r.Context(), tenantID, roomID, req.MemberID, req.Body)
	if err != nil {
		h.writePostFailure(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, toMessageResponse(msg))
}

// writePostFailure answers a request whose message could not be posted. Both
// ways of acquiring a turn — AppendMessage and ReserveTurn — reject with the
// same errors, and share this so an addressed message and a broadcast one
// report an identical room state identically.
func (h *Handler) writePostFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, room.ErrNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, room.ErrRoomTerminated):
		writeError(w, http.StatusConflict, "room is terminated")
	case errors.Is(err, room.ErrTurnBudgetExceeded):
		writeError(w, http.StatusConflict, "room turn budget exceeded")
	case errors.Is(err, room.ErrTurnReserved):
		// Not out of turns — the remaining ones are held by a delivery still
		// in flight. The room is still open, so this is worth retrying, unlike
		// an exhausted budget.
		writeError(w, http.StatusConflict, "the room's remaining turns are in flight; retry once delivery completes")
	case errors.Is(err, room.ErrMemberNotFound):
		writeError(w, http.StatusBadRequest, "member not found in room")
	default:
		h.logger.Error("post message: store error", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// postAddressedMessage handles a message addressed to another member: it
// delivers the message as a proxied call to that member's agent and, once
// delivery is confirmed, records it. It always writes a response — there is no
// outcome it hands back to the caller, precisely so an addressed message can
// never end up on the plain append path with no call made.
//
// The whole thing runs under a turn reservation, which is what makes delivery
// and recording safe as a pair. Confirming a call can take tens of seconds;
// between a bare "may this member post?" check and the eventual write, a
// concurrent request could consume the room's last turn or terminate the room,
// and the message would be refused after the recipient's agent had already
// been called — a real, possibly billed call with nothing in the transcript to
// show for it. Reserving the turn up front runs every check AppendMessage
// would run AND holds the turn across the delivery, so a confirmed call always
// lands.
func (h *Handler) postAddressedMessage(w http.ResponseWriter, r *http.Request, tenantID, roomID string, req postMessageRequest) {
	res, err := h.store.ReserveTurn(r.Context(), tenantID, roomID, room.PendingMessage{
		MemberID:   req.MemberID,
		ToMemberID: req.ToMemberID,
		Body:       req.Body,
	})
	if err != nil {
		// The room refused the turn, so nothing is delivered and nothing is
		// recorded. Reported exactly as the same refusal on a broadcast
		// message would be — but NOT by retrying as one. ErrTurnReserved in
		// particular is transient: a retry as a broadcast could well succeed,
		// turning a message meant for another agent into an ordinary one that
		// never left the room.
		h.writePostFailure(w, err)
		return
	}

	// A reservation must be resolved exactly once, or the room holds a turn it
	// can never spend. Everything below either commits it or falls into here.
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := h.store.ReleaseTurn(r.Context(), res); err != nil {
			h.logger.Error("post message: release turn", "err", err)
		}
	}()

	if !h.deliverToMember(w, r, tenantID, roomID, req) {
		return
	}

	// Delivered and confirmed. Recording it must not be abandoned because the
	// caller hung up mid-delivery — hanging up does not un-call the recipient's
	// agent, and the transcript has to say the call happened. Hence a context
	// detached from the request's cancellation.
	msg, err := h.store.CommitTurn(context.WithoutCancel(r.Context()), res)
	if err != nil {
		h.logger.Error("post message: record delivered message", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	committed = true

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
// Callers must hold a turn reservation for the message (see Store.ReserveTurn)
// before calling this — it performs a real side effect on another member's
// agent, and that side effect must be both permitted and recordable.
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

	result, callErr := h.gateway.Call(r.Context(), tenantID, toAgentID, payload)

	// What happened to the call is recorded on a context detached from the
	// request's: the call reached the recipient (or didn't) whether or not the
	// caller is still waiting, and an event log that drops outcomes when a
	// client disconnects is not one you can audit against.
	recordCtx := context.WithoutCancel(r.Context())

	if callErr == nil {
		// Confirmed delivered. Record the gateway's invocation ID so this
		// room's transcript can be reconciled against the gateway's own
		// invocation record when debugging.
		if err := h.store.RecordDelivery(recordCtx, tenantID, roomID, req.MemberID, req.ToMemberID, toAgentID, result.InvocationID); err != nil {
			h.logger.Error("post message: record delivery", "err", err)
		}
		return true
	}

	class := gateway.ErrorClassUpstreamFailure
	var ce *gateway.CallError
	if errors.As(callErr, &ce) {
		class = ce.Class
	}
	if err := h.store.RecordDeliveryFailure(recordCtx, tenantID, roomID, req.MemberID, req.ToMemberID, toAgentID, string(class)); err != nil {
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
	case gateway.ErrorClassTenantMismatch:
		// Nothing was sent: this deployment's gateway credential acts for a
		// different tenant, so the call could only have gone out misattributed.
		// A deployment problem, not a request problem — hence 5xx.
		h.logger.Error("post message: gateway client cannot act for this tenant",
			"tenant", tenantID, "room", roomID)
		writeError(w, http.StatusInternalServerError, "addressed messages are not available for this tenant")
	default:
		writeError(w, http.StatusBadGateway, "message could not be delivered: gateway upstream failure")
	}
	return false
}
