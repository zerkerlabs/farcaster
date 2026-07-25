package room

import (
	"context"
	"sync"
	"time"

	"github.com/zerkerlabs/farcaster/rooms/internal/resource"
)

// Store is a thread-safe, tenant-scoped, in-memory Room store. It holds rooms,
// their members, and their messages; every method takes a tenant ID and scopes
// reads and mutations to it.
type Store struct {
	mu       sync.RWMutex
	rooms    map[string]map[string]*Room // tenantID -> roomID -> *Room
	messages map[string][]*Message       // roomID -> ordered messages
}

// NewStore returns an empty Store ready for use.
func NewStore() *Store {
	return &Store{
		rooms:    make(map[string]map[string]*Room),
		messages: make(map[string][]*Message),
	}
}

// CreateRoom creates a new room under tenantID with the given goal. The room
// starts in StateOpen with no members.
func (s *Store) CreateRoom(ctx context.Context, tenantID, goal string) (*Room, error) {
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
		ID:        id,
		TenantID:  tenantID,
		Goal:      goal,
		State:     StateOpen,
		CreatedAt: time.Now().UTC(),
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

// AddMember seats an agent in a room. tenantID scopes the room lookup — it
// returns ErrNotFound if roomID does not exist or belongs to another tenant.
// agentTenantID is the tenant that owns agentID; if it differs from the room's
// tenant, the add is rejected with ErrTenantMismatch (rooms are single-tenant).
func (s *Store) AddMember(ctx context.Context, tenantID, roomID, agentID, agentTenantID string) (*Member, error) {
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
	if agentTenantID != r.TenantID {
		return nil, ErrTenantMismatch
	}

	m := &Member{
		ID:       id,
		AgentID:  agentID,
		JoinedAt: time.Now().UTC(),
	}
	r.Members = append(r.Members, m)
	return cloneMember(m), nil
}

// AppendMessage records a message in a room. Returns ErrNotFound if roomID does
// not exist or belongs to a different tenant.
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

	if _, err := s.find(tenantID, roomID); err != nil {
		return nil, err
	}

	m := &Message{
		ID:        id,
		MemberID:  memberID,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	}
	s.messages[roomID] = append(s.messages[roomID], m)
	return cloneMessage(m), nil
}

// Messages returns a room's messages in append order. Returns ErrNotFound if
// roomID does not exist or belongs to a different tenant.
func (s *Store) Messages(ctx context.Context, tenantID, roomID string) ([]*Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, err := s.find(tenantID, roomID); err != nil {
		return nil, err
	}

	msgs := s.messages[roomID]
	out := make([]*Message, len(msgs))
	for i, m := range msgs {
		out[i] = cloneMessage(m)
	}
	return out, nil
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
// through a shared Members slice.
func cloneRoom(r *Room) *Room {
	c := *r
	if r.Members != nil {
		c.Members = make([]*Member, len(r.Members))
		for i, m := range r.Members {
			c.Members[i] = cloneMember(m)
		}
	}
	return &c
}

func cloneMember(m *Member) *Member {
	c := *m
	return &c
}

func cloneMessage(m *Message) *Message {
	c := *m
	return &c
}
