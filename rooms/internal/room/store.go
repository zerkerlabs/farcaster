package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/resource"
)

// Store is a thread-safe, tenant-scoped, in-memory Room store. It holds rooms
// and their members; a room's messages are not stored separately — they are
// replayed from its event log on read.
type Store struct {
	mu    sync.RWMutex
	rooms map[string]map[string]*Room // tenantID -> roomID -> *Room
	// pending holds turns that have been reserved but not yet spent, keyed by
	// room ID then reservation ID (see ReserveTurn). A reserved turn counts
	// against its room's budget exactly like a posted message does, so two
	// concurrent posts can never spend the same last turn.
	//
	// Reservations live here rather than on Room so a cloned Room handed to a
	// caller carries no in-flight bookkeeping.
	pending map[string]map[string]*Reservation
}

// NewStore returns an empty Store ready for use.
func NewStore() *Store {
	return &Store{
		rooms:   make(map[string]map[string]*Room),
		pending: make(map[string]map[string]*Reservation),
	}
}

// CreateRoom creates a new room under tenantID with the given goal and the
// default turn budget. The room starts in StateOpen with no members.
func (s *Store) CreateRoom(ctx context.Context, tenantID, goal string) (*Room, error) {
	return s.createRoom(ctx, tenantID, goal, DefaultTurnBudget)
}

// CreateRoomWithBudget is CreateRoom with an explicit, positive turn budget in
// place of the default.
func (s *Store) CreateRoomWithBudget(ctx context.Context, tenantID, goal string, turnBudget int) (*Room, error) {
	if turnBudget <= 0 {
		return nil, fmt.Errorf("turn budget must be positive, got %d", turnBudget)
	}
	return s.createRoom(ctx, tenantID, goal, turnBudget)
}

func (s *Store) createRoom(ctx context.Context, tenantID, goal string, turnBudget int) (*Room, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := resource.New("rom")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r := &Room{
		ID:         id,
		TenantID:   tenantID,
		Goal:       goal,
		State:      StateOpen,
		CreatedAt:  time.Now().UTC(),
		TurnBudget: turnBudget,
	}
	s.bucket(tenantID)[id] = r
	return cloneRoom(r), nil
}

// GetRoom fetches one room by ID. Returns ErrNotFound if the ID does not exist
// or belongs to a different tenant.
func (s *Store) GetRoom(ctx context.Context, tenantID, roomID string) (*Room, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	return cloneRoom(r), nil
}

// ListRooms returns every room belonging to tenantID.
func (s *Store) ListRooms(ctx context.Context, tenantID string) ([]*Room, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rooms := make([]*Room, 0, len(s.rooms[tenantID]))
	for _, r := range s.rooms[tenantID] {
		rooms = append(rooms, cloneRoom(r))
	}
	return rooms, nil
}

// AddMember seats an agent in a room, carrying startingContext — the
// onboarding context the caller assembled from the agent's memory scope plus
// any documents supplied on the request (rooms/internal/memory); pass nil for
// a member with no onboarding context. tenantID scopes the room lookup — it
// returns ErrNotFound if roomID does not exist or belongs to another tenant.
// agentTenantID is the tenant that owns agentID; if it differs from the room's
// tenant, the add is rejected with ErrTenantMismatch (rooms are single-tenant).
// Returns ErrRoomTerminated if the room has already reached a terminal state.
func (s *Store) AddMember(ctx context.Context, tenantID, roomID, agentID, agentTenantID string, startingContext []string) (*Member, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := resource.New("mem")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	if r.State != StateOpen {
		return nil, ErrRoomTerminated
	}
	if agentTenantID != r.TenantID {
		return nil, ErrTenantMismatch
	}

	m := &Member{
		ID:              id,
		AgentID:         agentID,
		JoinedAt:        time.Now().UTC(),
		StartingContext: append([]string(nil), startingContext...),
	}
	r.Members = append(r.Members, m)
	s.appendEvent(r, EventMemberJoined, MemberJoinedPayload{Member: m})
	return cloneMember(m), nil
}

// AppendMessage records a message in a room, authored by one of its members.
// Posting a message consumes one turn; if doing so would exceed the room's
// turn budget, the message is rejected with ErrTurnBudgetExceeded and the
// room transitions to StateAbandoned instead. Returns ErrNotFound if roomID
// does not exist or belongs to a different tenant, ErrMemberNotFound if
// memberID is not seated in that room, ErrRoomTerminated if the room has
// already reached a terminal state, and ErrTurnReserved if its remaining turns
// are held by deliveries still in flight (see ReserveTurn) — a retryable
// conflict that leaves the room open, since those turns may yet be released.
func (s *Store) AppendMessage(ctx context.Context, tenantID, roomID, memberID, body string) (*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := resource.New("msg")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	if err := s.canPost(r, memberID); err != nil {
		s.abandonIfOutOfTurns(r, err)
		return nil, err
	}

	m := &Message{
		ID:        id,
		MemberID:  memberID,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	s.appendEvent(r, EventMessagePosted, MessagePostedPayload{Message: m})
	return cloneMessage(m), nil
}

// PendingMessage is the message a turn reservation is taken for. ToMemberID is
// empty for a message that is not addressed to a specific member.
type PendingMessage struct {
	MemberID   string
	ToMemberID string
	Body       string
}

// Reservation is a claim on one of a room's turns, held while its caller
// performs a side effect that must not happen unless the message can actually
// be recorded — delivering it to another member's agent through the gateway.
// Resolve it exactly once, with CommitTurn or ReleaseTurn; a reservation that
// is never resolved holds a turn the room can never spend.
type Reservation struct {
	// messageID is minted at reservation time and doubles as the reservation's
	// own ID, so committing cannot fail on ID generation.
	messageID string
	tenantID  string
	roomID    string
	msg       PendingMessage
}

// ReserveTurn claims one of a room's turns for msg, running exactly the checks
// AppendMessage runs — the room exists and is visible to this tenant, it is
// open, msg.MemberID is seated in it, and the budget has room for one more turn
// — and, if they pass, holding that turn until the reservation is resolved.
//
// It exists so a caller with a side effect to perform before recording a
// message can make that side effect safe in both directions. Firing it for a
// request the room would reject means it happened for a request that was never
// valid; firing it and only then finding the room out of turns or terminated by
// a concurrent request means it happened with no record in the transcript.
// Reserving the turn up front closes both: the checks run before the side
// effect, and the turn is held across it, so a committed message cannot be
// refused by anything that arrives in the meantime.
//
// Reserving is how the addressed-message path acquires its turn, so it carries
// the same consequences AppendMessage does: a room that is genuinely out of
// turns is abandoned here too, rather than letting agents loop indefinitely.
// A turn that is merely reserved by another delivery is different — that one
// may still be released unspent — so it is refused with ErrTurnReserved and
// leaves the room open.
func (s *Store) ReserveTurn(ctx context.Context, tenantID, roomID string, msg PendingMessage) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := resource.New("msg")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	if err := s.canPost(r, msg.MemberID); err != nil {
		s.abandonIfOutOfTurns(r, err)
		return nil, err
	}

	res := &Reservation{messageID: id, tenantID: tenantID, roomID: roomID, msg: msg}
	if s.pending[roomID] == nil {
		s.pending[roomID] = make(map[string]*Reservation)
	}
	s.pending[roomID][id] = res
	return res, nil
}

// CommitTurn spends a reserved turn, recording the message it was reserved for.
//
// It is deliberately unconditional. By the time a caller commits, the side
// effect the reservation was protecting has already happened — the message was
// delivered to the recipient's agent, with whatever policy and payment
// consequences that carried — so the transcript must record it even if a
// concurrent request terminated the room or exhausted its budget in the
// meantime. A confirmed delivery with no message in the transcript is the worse
// outcome by far: the recipient got the call and the room denies it.
//
// For the same reason it does not honour ctx cancellation. A caller that hung
// up mid-delivery does not un-deliver the call, so ctx is accepted only for
// symmetry with the rest of Store.
//
// Returns ErrReservationNotHeld if res was already committed or released, and
// ErrNotFound if its room no longer exists.
func (s *Store) CommitTurn(_ context.Context, res *Reservation) (*Message, error) {
	if res == nil {
		return nil, ErrReservationNotHeld
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(res.tenantID, res.roomID)
	if err != nil {
		return nil, err
	}
	if !s.dropPending(res) {
		return nil, ErrReservationNotHeld
	}

	m := &Message{
		ID:         res.messageID,
		MemberID:   res.msg.MemberID,
		ToMemberID: res.msg.ToMemberID,
		Body:       res.msg.Body,
		CreatedAt:  time.Now().UTC(),
	}
	s.appendEvent(r, EventMessagePosted, MessagePostedPayload{Message: m})
	return cloneMessage(m), nil
}

// ReleaseTurn gives a reserved turn back unspent: the side effect it was
// protecting did not happen, so nothing is recorded and the room is free to
// spend the turn on something else. Like CommitTurn it ignores ctx
// cancellation — a released turn that stayed reserved would be lost for the
// life of the room.
//
// Returns ErrReservationNotHeld if res was already committed or released.
func (s *Store) ReleaseTurn(_ context.Context, res *Reservation) error {
	if res == nil {
		return ErrReservationNotHeld
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dropPending(res) {
		return ErrReservationNotHeld
	}
	return nil
}

// canPost reports whether memberID may post one message to r right now: the
// room is open, memberID is seated in it, and the budget has room for one more
// turn. Caller must hold s.mu (any lock).
func (s *Store) canPost(r *Room, memberID string) error {
	if r.State != StateOpen {
		return ErrRoomTerminated
	}

	// A message must be attributable to someone actually in the room.
	// Without this a caller could persist a message pointing at a member ID
	// that never existed, or one belonging to a different room entirely.
	if !hasMember(r, memberID) {
		return ErrMemberNotFound
	}

	// Turns already spent are gone for good — running past them is what
	// abandons a room. Turns merely reserved might still come back (a delivery
	// that fails releases its turn unspent), so being blocked by one is a
	// retryable conflict, not the end of the room. Distinguishing them means a
	// room can never be abandoned on account of a call that never landed.
	if turnsConsumed(r)+1 > r.TurnBudget {
		return ErrTurnBudgetExceeded
	}
	if s.turnsSpent(r)+1 > r.TurnBudget {
		return ErrTurnReserved
	}
	return nil
}

// abandonIfOutOfTurns terminates r when err says its turn budget is genuinely
// spent — the rule that stops agents looping indefinitely. Both ways of
// acquiring a turn (AppendMessage and ReserveTurn) share it, so a room is
// abandoned by the same condition however the post arrived.
//
// ErrTurnReserved deliberately does not trigger it: those turns are held by
// deliveries still in flight and may yet be released, so a call that never
// landed must not be able to kill a room. Caller must hold s.mu (write lock).
func (s *Store) abandonIfOutOfTurns(r *Room, err error) {
	if !errors.Is(err, ErrTurnBudgetExceeded) {
		return
	}
	r.State = StateAbandoned
	s.appendEvent(r, EventRoomTerminated, RoomTerminatedPayload{State: StateAbandoned})
}

// dropPending removes res from the pending set, reporting whether it was still
// there — false means it was already committed or released, and the caller must
// not act on it twice. Caller must hold s.mu (write lock).
func (s *Store) dropPending(res *Reservation) bool {
	held, ok := s.pending[res.roomID]
	if !ok {
		return false
	}
	if _, ok := held[res.messageID]; !ok {
		return false
	}
	delete(held, res.messageID)
	if len(held) == 0 {
		delete(s.pending, res.roomID)
	}
	return true
}

// CompleteRoom explicitly marks a room's goal as met. Returns ErrNotFound if
// roomID does not exist or belongs to a different tenant, and
// ErrRoomTerminated if the room has already reached a terminal state.
func (s *Store) CompleteRoom(ctx context.Context, tenantID, roomID string) (*Room, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	if r.State != StateOpen {
		return nil, ErrRoomTerminated
	}

	r.State = StateCompleted
	s.appendEvent(r, EventRoomTerminated, RoomTerminatedPayload{State: StateCompleted})
	return cloneRoom(r), nil
}

// MemberAgentID returns the AgentID of the member identified by memberID
// within roomID, so a caller can resolve the gateway agent to deliver an
// addressed message to before making the proxied call
// (rooms/internal/gateway). Returns ErrNotFound if roomID does not exist or
// belongs to a different tenant, and ErrMemberNotFound if memberID is not
// seated in that room.
func (s *Store) MemberAgentID(ctx context.Context, tenantID, roomID, memberID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return "", err
	}
	for _, m := range r.Members {
		if m.ID == memberID {
			return m.AgentID, nil
		}
	}
	return "", ErrMemberNotFound
}

// RecordDeliveryFailure appends an EventDeliveryFailed event to a room's log:
// a message from fromMemberID addressed to toMemberID could not be delivered
// as a proxied call to toAgentID. It does not consume a turn and does not
// append a message — a failed call must never look like a delivered one
// (the caller is responsible for not calling AppendMessage in this case).
// Returns ErrNotFound if roomID does not exist or belongs to a different
// tenant.
func (s *Store) RecordDeliveryFailure(ctx context.Context, tenantID, roomID, fromMemberID, toMemberID, toAgentID, class string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return err
	}

	s.appendEvent(r, EventDeliveryFailed, DeliveryFailedPayload{
		FromMemberID: fromMemberID,
		ToMemberID:   toMemberID,
		ToAgentID:    toAgentID,
		Class:        class,
	})
	return nil
}

// RecordDelivery appends an EventMessageDelivered event recording that a
// message from fromMemberID addressed to toMemberID was confirmed delivered as
// a proxied call to toAgentID, carrying the gateway's invocationID so the room
// history can be reconciled against the gateway's invocation record.
//
// It does not append the message itself — the caller does that — so a delivery
// and the message it delivered stay separately attributable in the log.
// Returns ErrNotFound if roomID does not exist or belongs to a different
// tenant.
func (s *Store) RecordDelivery(ctx context.Context, tenantID, roomID, fromMemberID, toMemberID, toAgentID, invocationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return err
	}

	s.appendEvent(r, EventMessageDelivered, MessageDeliveredPayload{
		FromMemberID: fromMemberID,
		ToMemberID:   toMemberID,
		ToAgentID:    toAgentID,
		InvocationID: invocationID,
	})
	return nil
}

// hasMember reports whether memberID is seated in r.
func hasMember(r *Room, memberID string) bool {
	for _, m := range r.Members {
		if m.ID == memberID {
			return true
		}
	}
	return false
}

// Messages returns a room's transcript: its messages in append order,
// replayed from its event log. Returns ErrNotFound if roomID does not exist
// or belongs to a different tenant.
func (s *Store) Messages(ctx context.Context, tenantID, roomID string) ([]*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}
	return transcript(r), nil
}

// Events returns a room's full event log in sequence order. Returns
// ErrNotFound if roomID does not exist or belongs to a different tenant.
func (s *Store) Events(ctx context.Context, tenantID, roomID string) ([]*Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	r, err := s.find(tenantID, roomID)
	if err != nil {
		return nil, err
	}

	out := make([]*Event, len(r.Events))
	for i, ev := range r.Events {
		out[i] = cloneEvent(ev)
	}
	return out, nil
}

// transcript replays r's event log into its ordered message history. A room
// keeps no separate message list, so this is the only source of the
// transcript. Caller must hold s.mu (any lock).
func transcript(r *Room) []*Message {
	msgs := make([]*Message, 0, len(r.Events))
	for _, ev := range r.Events {
		if ev.Kind != EventMessagePosted {
			continue
		}
		p := ev.Payload.(MessagePostedPayload)
		msgs = append(msgs, cloneMessage(p.Message))
	}
	return msgs
}

// turnsSpent counts how many of r's turns are no longer available: those its
// event log has already spent, plus those currently reserved but not yet
// committed (see ReserveTurn). A reserved turn has to count, or a post that
// arrives while a delivery is in flight could spend the same last turn twice.
// Caller must hold s.mu (any lock).
func (s *Store) turnsSpent(r *Room) int {
	return turnsConsumed(r) + len(s.pending[r.ID])
}

// turnsConsumed counts how many turns r's event log has already spent.
// Posting a message is the only turn-consuming action. Caller must hold s.mu
// (any lock).
func turnsConsumed(r *Room) int {
	n := 0
	for _, ev := range r.Events {
		if ev.Kind == EventMessagePosted {
			n++
		}
	}
	return n
}

// appendEvent appends a new Event of the given kind and payload to r's log,
// assigning the next contiguous sequence number. Caller must hold s.mu (write
// lock).
func (s *Store) appendEvent(r *Room, kind EventKind, payload any) {
	r.Events = append(r.Events, &Event{
		Sequence:  len(r.Events) + 1,
		Kind:      kind,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	})
}

// bucket returns (and lazily initialises) the per-tenant room map. Caller must
// hold s.mu (write lock).
func (s *Store) bucket(tenantID string) map[string]*Room {
	if s.rooms[tenantID] == nil {
		s.rooms[tenantID] = make(map[string]*Room)
	}
	return s.rooms[tenantID]
}

// find looks up a room by tenant and ID. Caller must hold s.mu (any lock).
func (s *Store) find(tenantID, roomID string) (*Room, error) {
	bucket, ok := s.rooms[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	r, ok := bucket[roomID]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

// cloneRoom returns a deep copy of r so callers cannot mutate stored state
// through a shared Members or Events slice.
func cloneRoom(r *Room) *Room {
	c := *r
	if r.Members != nil {
		c.Members = make([]*Member, len(r.Members))
		for i, m := range r.Members {
			c.Members[i] = cloneMember(m)
		}
	}
	if r.Events != nil {
		c.Events = make([]*Event, len(r.Events))
		for i, ev := range r.Events {
			c.Events[i] = cloneEvent(ev)
		}
	}
	return &c
}

func cloneMember(m *Member) *Member {
	c := *m
	if m.StartingContext != nil {
		c.StartingContext = append([]string(nil), m.StartingContext...)
	}
	return &c
}

func cloneMessage(m *Message) *Message {
	c := *m
	return &c
}

// cloneEvent returns a deep copy of ev, including any pointer held in its
// Payload, so callers cannot mutate stored state through a returned Event.
func cloneEvent(ev *Event) *Event {
	c := *ev
	switch p := ev.Payload.(type) {
	case MemberJoinedPayload:
		c.Payload = MemberJoinedPayload{Member: cloneMember(p.Member)}
	case MessagePostedPayload:
		c.Payload = MessagePostedPayload{Message: cloneMessage(p.Message)}
	default:
		// RoomTerminatedPayload (and any future value-only payload) holds no
		// pointers, so the shallow copy above already isolated it.
	}
	return &c
}
