package policy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/gateway/gateway/internal/resource"
)

// decisionCols is the canonical column list for policy_decisions reads, shared
// by Insert's RETURNING and ListRecent's SELECT.
const decisionCols = `id, tenant_id, agent_id, protocol, mcp_tool, action, matched_rule, reason, created_at`

// PostgresDecisionStore is a PostgreSQL-backed, tenant-scoped DecisionStore.
// Use NewPostgresDecisionStore to construct one; do not copy by value.
type PostgresDecisionStore struct {
	pool *pgxpool.Pool
}

// NewPostgresDecisionStore returns a PostgresDecisionStore that uses pool for
// all queries. pool must already be open; the caller closes it.
func NewPostgresDecisionStore(pool *pgxpool.Pool) *PostgresDecisionStore {
	return &PostgresDecisionStore{pool: pool}
}

// rowScanner abstracts pgx.Row and a pgx.Rows cursor so one scan helper serves
// both Insert (single RETURNING row) and ListRecent (a cursor).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDecision(row rowScanner) (*StoredDecision, error) {
	var (
		d       StoredDecision
		action  string
		mcpTool *string
	)
	if err := row.Scan(
		&d.ID, &d.TenantID, &d.AgentID, &d.Protocol, &mcpTool,
		&action, &d.MatchedRule, &d.Reason, &d.CreatedAt,
	); err != nil {
		return nil, err
	}
	d.Action = Action(action)
	d.MCPTool = mcpTool
	return &d, nil
}

// Insert implements DecisionStore.
func (s *PostgresDecisionStore) Insert(ctx context.Context, rd RecordedDecision) (*StoredDecision, error) {
	id, err := resource.New(decisionIDPrefix)
	if err != nil {
		return nil, err
	}

	row := s.pool.QueryRow(
		ctx, `
		INSERT INTO policy_decisions (id, tenant_id, agent_id, protocol, mcp_tool, action, matched_rule, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		RETURNING `+decisionCols,
		id, rd.TenantID, rd.AgentID, rd.Protocol, rd.MCPTool,
		string(rd.Decision.Action), rd.Decision.MatchedRule, rd.Decision.Reason,
	)
	d, err := scanDecision(row)
	if err != nil {
		return nil, fmt.Errorf("insert policy decision: %w", err)
	}
	return d, nil
}

// ListRecent implements DecisionStore. Tenant scoping is the WHERE clause: a
// row belonging to another tenant is simply never selected (invariant #2).
func (s *PostgresDecisionStore) ListRecent(ctx context.Context, tenantID string, limit int) ([]*StoredDecision, error) {
	if limit < 1 {
		limit = decisionDefaultLimit
	}

	rows, err := s.pool.Query(
		ctx,
		`SELECT `+decisionCols+` FROM policy_decisions WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		tenantID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list policy decisions: %w", err)
	}
	defer rows.Close()

	var out []*StoredDecision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("scan policy decision: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy decisions: %w", err)
	}
	return out, nil
}
