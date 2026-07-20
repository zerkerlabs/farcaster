package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/gateway/internal/resource"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE code for unique_violation.
const pgUniqueViolation = "23505"

// PostgresStore is a PostgreSQL-backed, tenant-scoped implementation of
// AgentStore. Use NewPostgresStore to construct one; do not copy by value.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore that uses pool for all queries.
// pool must already be open; the caller is responsible for closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// selectCols is the canonical column list for all SELECT and RETURNING clauses.
const selectCols = `id, tenant_id, name, description, tags, metadata, status, created_by, created_at, updated_at, upstream_url, credential_ref, capture_body, suspended, invocation_rate_limit, invocation_burst, emit_receipts, protocol, mcp_transport, mcp_protocol_version, price_amount, price_asset, price_network, price_pay_to, price_tools`

// rowScanner abstracts pgx.Row and pgx.Rows so scanAgent can be used in both
// QueryRow and rows.Next contexts without duplicating scan logic.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAgent reads one row into an Agent. Tags and metadata are normalised so
// that empty values round-trip as nil, matching MemoryStore behaviour. Nullable
// text columns (upstream_url, credential_ref) are scanned into *string — nil
// when the column is NULL. Nullable numeric policy columns are scanned into
// pointer types — nil when NULL (no per-agent override).
func scanAgent(row rowScanner) (*Agent, error) {
	var (
		a                   Agent
		tags                pgtype.FlatArray[string]
		metaJSON            []byte
		status              string
		upstreamURL         *string
		credentialRef       *string
		invocationRateLimit *float64
		invocationBurst     *int
		protocol            *string
		mcpTransport        *string
		mcpProtocolVersion  *string
		priceAmount         *string
		priceAsset          *string
		priceNetwork        *string
		pricePayTo          *string
		priceTools          []byte
	)
	if err := row.Scan(
		&a.ID, &a.TenantID, &a.Name, &a.Description,
		&tags, &metaJSON, &status, &a.CreatedBy,
		&a.CreatedAt, &a.UpdatedAt,
		&upstreamURL, &credentialRef, &a.CaptureBody,
		&a.Suspended, &invocationRateLimit, &invocationBurst, &a.EmitReceipts,
		&protocol, &mcpTransport, &mcpProtocolVersion,
		&priceAmount, &priceAsset, &priceNetwork, &pricePayTo, &priceTools,
	); err != nil {
		return nil, err
	}
	a.Status = Status(status)
	a.UpstreamURL = upstreamURL
	a.CredentialRef = credentialRef
	a.InvocationRateLimit = invocationRateLimit
	a.InvocationBurst = invocationBurst

	// protocol has no NOT NULL/default at the DB level (migration 010), so
	// pre-migration rows scan as NULL here; normalizeProtocol maps that (and
	// an empty string) onto "http" so existing agents are unaffected.
	var protoStr string
	if protocol != nil {
		protoStr = *protocol
	}
	a.Protocol = normalizeProtocol(protoStr)
	a.MCPTransport = mcpTransport
	a.MCPProtocolVersion = mcpProtocolVersion

	// Pricing is stored across five nullable columns; price_amount being
	// non-NULL is the sentinel that the agent is priced (spec 0005). The
	// price_tools JSON map is optional even for a priced agent.
	if priceAmount != nil {
		p := &Pricing{Amount: *priceAmount}
		if priceAsset != nil {
			p.Asset = *priceAsset
		}
		if priceNetwork != nil {
			p.Network = *priceNetwork
		}
		if pricePayTo != nil {
			p.PayTo = *pricePayTo
		}
		if len(priceTools) > 0 {
			if err := json.Unmarshal(priceTools, &p.Tools); err != nil {
				return nil, fmt.Errorf("unmarshal price_tools: %w", err)
			}
			if len(p.Tools) == 0 {
				p.Tools = nil
			}
		}
		a.Pricing = p
	}

	if len(tags) > 0 {
		a.Tags = []string(tags)
	}

	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &a.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
	}

	return &a, nil
}

// normTags returns a non-nil slice so the DB never receives a NULL for the
// NOT NULL tags column.
func normTags(t []string) pgtype.FlatArray[string] {
	if t == nil {
		return pgtype.FlatArray[string]{}
	}
	return pgtype.FlatArray[string](t)
}

// pricingColumns explodes a *Pricing into the five nullable column values used
// by the INSERT and UPDATE statements. A nil *Pricing yields all-NULL columns
// (unpriced). An empty/nil Tools map yields a NULL price_tools column.
func pricingColumns(p *Pricing) (amount, asset, network, payTo *string, tools []byte, err error) {
	if p == nil {
		return nil, nil, nil, nil, nil, nil
	}
	amount = &p.Amount
	asset = &p.Asset
	network = &p.Network
	payTo = &p.PayTo
	if len(p.Tools) > 0 {
		tools, err = json.Marshal(p.Tools)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("marshal price_tools: %w", err)
		}
	}
	return amount, asset, network, payTo, tools, nil
}

// marshalMeta JSON-encodes metadata for storage. A nil map serialises as "{}".
func marshalMeta(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return b, nil
}

// Create implements AgentStore.
func (s *PostgresStore) Create(ctx context.Context, tenantID string, a *Agent) error {
	id, err := resource.New("agt")
	if err != nil {
		return err
	}

	metaJSON, err := marshalMeta(a.Metadata)
	if err != nil {
		return err
	}

	status := statusFromUpstreamURL(a.UpstreamURL)

	priceAmount, priceAsset, priceNetwork, pricePayTo, priceTools, err := pricingColumns(a.Pricing)
	if err != nil {
		return err
	}

	row := s.pool.QueryRow(
		ctx, `
		INSERT INTO agents
			(id, tenant_id, name, description, tags, metadata, status, created_by,
			 upstream_url, credential_ref, capture_body,
			 suspended, invocation_rate_limit, invocation_burst,
			 emit_receipts, protocol, mcp_transport, mcp_protocol_version,
			 price_amount, price_asset, price_network, price_pay_to, price_tools,
			 created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, NOW(), NOW())
		RETURNING `+selectCols,
		id, tenantID, a.Name, a.Description, normTags(a.Tags), metaJSON,
		string(status), a.CreatedBy, a.UpstreamURL, a.CredentialRef, a.CaptureBody,
		a.Suspended, a.InvocationRateLimit, a.InvocationBurst, a.EmitReceipts,
		normalizeProtocol(a.Protocol), a.MCPTransport, a.MCPProtocolVersion,
		priceAmount, priceAsset, priceNetwork, pricePayTo, priceTools,
	)

	got, err := scanAgent(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ErrNameConflict
		}
		return fmt.Errorf("create agent: %w", err)
	}
	*a = *got
	return nil
}

// Get implements AgentStore. Inactive (soft-deleted) agents are returned so
// that audit readers can still fetch them; they are excluded from List only.
func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (*Agent, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM agents WHERE id=$1 AND tenant_id=$2`,
		id, tenantID,
	)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get agent: %w", err)
	}
	return a, nil
}

// protocolFilterClause matches rows whose protocol equals $2, or every row
// when $2 is the empty string ("no filter"). protocol has no NOT NULL/default
// at the DB level (migration 010), so NULL and "" are folded onto "http" here
// the same way normalizeProtocol does in Go — pre-migration rows must match
// protocol=http.
const protocolFilterClause = `($2 = '' OR COALESCE(NULLIF(protocol, ''), 'http') = $2)`

// List implements AgentStore.
func (s *PostgresStore) List(ctx context.Context, tenantID string, page, perPage int, protocol string) ([]*Agent, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}

	var total int
	if err := s.pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM agents
		WHERE tenant_id=$1 AND status IN ('active', 'pending')
		  AND `+protocolFilterClause,
		tenantID, protocol,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count agents: %w", err)
	}
	if total == 0 {
		return []*Agent{}, 0, nil
	}

	offset := (page - 1) * perPage
	rows, err := s.pool.Query(
		ctx, `
		SELECT `+selectCols+`
		FROM agents
		WHERE tenant_id=$1 AND status IN ('active', 'pending')
		  AND `+protocolFilterClause+`
		ORDER BY created_at ASC, id ASC
		LIMIT $3 OFFSET $4`,
		tenantID, protocol, perPage, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var agents []*Agent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("scan agent: %w", scanErr)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list agents: %w", err)
	}

	if agents == nil {
		agents = []*Agent{}
	}
	return agents, total, nil
}

// Update implements AgentStore. It fetches the current record first so that
// fields absent from UpdateFields keep their stored values, avoiding dynamic
// SQL construction.
func (s *PostgresStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Agent, error) {
	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if current.Status == StatusInactive {
		return nil, ErrNotFound
	}

	name := current.Name
	if fields.Name != nil {
		name = *fields.Name
	}
	description := current.Description
	if fields.Description != nil {
		description = *fields.Description
	}
	tags := current.Tags
	if fields.Tags != nil {
		tags = *fields.Tags
	}
	meta := current.Metadata
	if fields.Metadata != nil {
		meta = *fields.Metadata
	}
	upstreamURL := current.UpstreamURL
	if fields.UpstreamURL != nil {
		upstreamURL = *fields.UpstreamURL
	}
	credentialRef := current.CredentialRef
	if fields.CredentialRef != nil {
		credentialRef = *fields.CredentialRef
	}
	captureBody := current.CaptureBody
	if fields.CaptureBody != nil {
		captureBody = *fields.CaptureBody
	}
	emitReceipts := current.EmitReceipts
	if fields.EmitReceipts != nil {
		emitReceipts = *fields.EmitReceipts
	}
	suspended := current.Suspended
	if fields.Suspended != nil {
		suspended = *fields.Suspended
	}
	invocationRateLimit := current.InvocationRateLimit
	if fields.InvocationRateLimit != nil {
		invocationRateLimit = *fields.InvocationRateLimit
	}
	invocationBurst := current.InvocationBurst
	if fields.InvocationBurst != nil {
		invocationBurst = *fields.InvocationBurst
	}
	protocol := current.Protocol
	if fields.Protocol != nil {
		protocol = normalizeProtocol(*fields.Protocol)
	}
	mcpTransport := current.MCPTransport
	if fields.MCPTransport != nil {
		mcpTransport = *fields.MCPTransport
	}
	mcpProtocolVersion := current.MCPProtocolVersion
	if fields.MCPProtocolVersion != nil {
		mcpProtocolVersion = *fields.MCPProtocolVersion
	}
	pricing := current.Pricing
	if fields.Pricing != nil {
		pricing = *fields.Pricing
	}

	newStatus := statusFromUpstreamURL(upstreamURL)

	metaJSON, err := marshalMeta(meta)
	if err != nil {
		return nil, err
	}

	priceAmount, priceAsset, priceNetwork, pricePayTo, priceTools, err := pricingColumns(pricing)
	if err != nil {
		return nil, err
	}

	row := s.pool.QueryRow(
		ctx, `
		UPDATE agents
		SET name=$3, description=$4, tags=$5, metadata=$6,
		    upstream_url=$7, credential_ref=$8, capture_body=$9, status=$10,
		    suspended=$11, invocation_rate_limit=$12, invocation_burst=$13,
		    emit_receipts=$14, protocol=$15, mcp_transport=$16,
		    mcp_protocol_version=$17, price_amount=$18, price_asset=$19,
		    price_network=$20, price_pay_to=$21, price_tools=$22, updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2 AND status IN ('active', 'pending')
		RETURNING `+selectCols,
		id, tenantID, name, description, normTags(tags), metaJSON,
		upstreamURL, credentialRef, captureBody, string(newStatus),
		suspended, invocationRateLimit, invocationBurst, emitReceipts,
		protocol, mcpTransport, mcpProtocolVersion,
		priceAmount, priceAsset, priceNetwork, pricePayTo, priceTools,
	)
	updated, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Concurrent soft-delete between GET and UPDATE.
			return nil, ErrNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrNameConflict
		}
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return updated, nil
}

// Delete implements AgentStore. The WHERE status IN ('active', 'pending')
// predicate means an already-inactive or missing/cross-tenant agent produces
// zero rows affected, which maps uniformly to ErrNotFound — existence is never
// revealed across tenants (security invariant #2, AGENTS.md).
func (s *PostgresStore) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(
		ctx, `
		UPDATE agents
		SET status='inactive', updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2 AND status IN ('active', 'pending')`,
		id, tenantID,
	)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
