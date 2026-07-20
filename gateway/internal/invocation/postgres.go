package invocation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkerlabs/farcaster/gateway/internal/resource"
)

// PostgresStore is a PostgreSQL-backed, tenant-scoped implementation of Store.
// Use NewPostgresStore to construct one; do not copy by value.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PostgresStore that uses pool for all queries.
// pool must already be open; the caller is responsible for closing it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// selectCols is the canonical column list for all SELECT and RETURNING clauses.
const selectCols = `id, tenant_id, agent_id, mode, status,
	created_at, updated_at, completed_at,
	upstream_status, latency_ms, req_size, resp_size, req_body, resp_body,
	ttft_ms, error_class, model, mcp_method, mcp_tool,
	payment_network, payment_asset, payment_amount, payment_payer, payment_nonce,
	settlement_status, settlement_tx_hash, settled_amount, operator_amount,
	facilitator_fee, settlement_attempts, settlement_reason, settled_at`

// rowScanner abstracts pgx.Row and pgx.Rows so scanInvocation can be used in
// both QueryRow and rows.Next contexts without duplicating scan logic.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanInvocation reads one row into an Invocation.
func scanInvocation(row rowScanner) (*Invocation, error) {
	var (
		inv                Invocation
		mode               string
		status             string
		completedAt        *time.Time
		upstreamStatus     *int
		latencyMS          *int64
		reqSize            *int64
		respSize           *int64
		reqBody            []byte
		respBody           []byte
		ttftMS             *int64
		errorClass         *string
		model              *string
		mcpMethod          *string
		mcpTool            *string
		paymentNetwork     *string
		paymentAsset       *string
		paymentAmount      *string
		paymentPayer       *string
		paymentNonce       *string
		settlementStatus   *string
		settlementTxHash   *string
		settledAmount      *string
		operatorAmount     *string
		facilitatorFee     *string
		settlementAttempts *int
		settlementReason   *string
		settledAt          *time.Time
	)
	if err := row.Scan(
		&inv.ID, &inv.TenantID, &inv.AgentID, &mode, &status,
		&inv.CreatedAt, &inv.UpdatedAt, &completedAt,
		&upstreamStatus, &latencyMS, &reqSize, &respSize, &reqBody, &respBody,
		&ttftMS, &errorClass, &model, &mcpMethod, &mcpTool,
		&paymentNetwork, &paymentAsset, &paymentAmount, &paymentPayer, &paymentNonce,
		&settlementStatus, &settlementTxHash, &settledAmount, &operatorAmount,
		&facilitatorFee, &settlementAttempts, &settlementReason, &settledAt,
	); err != nil {
		return nil, err
	}
	inv.Mode = Mode(mode)
	inv.Status = Status(status)
	inv.CompletedAt = completedAt
	inv.UpstreamStatus = upstreamStatus
	inv.LatencyMS = latencyMS
	inv.RequestSize = reqSize
	inv.ResponseSize = respSize
	inv.RequestBody = reqBody
	inv.ResponseBody = respBody
	inv.TTFTMS = ttftMS
	if errorClass != nil {
		ec := ErrorClass(*errorClass)
		inv.ErrorClass = &ec
	}
	inv.Model = model
	inv.MCPMethod = mcpMethod
	inv.MCPTool = mcpTool
	inv.PaymentNetwork = paymentNetwork
	inv.PaymentAsset = paymentAsset
	inv.PaymentAmount = paymentAmount
	inv.PaymentPayer = paymentPayer
	inv.PaymentNonce = paymentNonce
	if settlementStatus != nil {
		ss := SettlementStatus(*settlementStatus)
		inv.SettlementStatus = &ss
	}
	inv.SettlementTxHash = settlementTxHash
	inv.SettledAmount = settledAmount
	inv.OperatorAmount = operatorAmount
	inv.FacilitatorFee = facilitatorFee
	inv.SettlementAttempts = settlementAttempts
	inv.SettlementReason = settlementReason
	inv.SettledAt = settledAt
	return &inv, nil
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, tenantID string, inv *Invocation) error {
	id, err := resource.New("inv")
	if err != nil {
		return err
	}

	var errorClassStr *string
	if inv.ErrorClass != nil {
		ec := string(*inv.ErrorClass)
		errorClassStr = &ec
	}

	var settlementStatusStr *string
	if inv.SettlementStatus != nil {
		ss := string(*inv.SettlementStatus)
		settlementStatusStr = &ss
	}

	row := s.pool.QueryRow(
		ctx, `
		INSERT INTO invocations
			(id, tenant_id, agent_id, mode, status, req_size, req_body,
			 ttft_ms, error_class, model, mcp_method, mcp_tool,
			 payment_network, payment_asset, payment_amount, payment_payer, payment_nonce,
			 settlement_status, settlement_tx_hash, settled_amount, operator_amount,
			 facilitator_fee, settlement_attempts, settlement_reason, settled_at,
			 created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
		        $18, $19, $20, $21, $22, $23, $24, $25, NOW(), NOW())
		RETURNING `+selectCols,
		id, tenantID, inv.AgentID, string(inv.Mode), string(inv.Status), inv.RequestSize, inv.RequestBody,
		inv.TTFTMS, errorClassStr, inv.Model, inv.MCPMethod, inv.MCPTool,
		inv.PaymentNetwork, inv.PaymentAsset, inv.PaymentAmount, inv.PaymentPayer, inv.PaymentNonce,
		settlementStatusStr, inv.SettlementTxHash, inv.SettledAmount, inv.OperatorAmount,
		inv.FacilitatorFee, inv.SettlementAttempts, inv.SettlementReason, inv.SettledAt,
	)

	got, err := scanInvocation(row)
	if err != nil {
		return fmt.Errorf("create invocation: %w", err)
	}
	*inv = *got
	return nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (*Invocation, error) {
	row := s.pool.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM invocations WHERE id=$1 AND tenant_id=$2`,
		id, tenantID,
	)
	inv, err := scanInvocation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get invocation: %w", err)
	}
	return inv, nil
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, tenantID, agentID string, page, perPage int) ([]*Invocation, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}

	var total int
	if err := s.pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM invocations WHERE tenant_id=$1 AND agent_id=$2`,
		tenantID, agentID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count invocations: %w", err)
	}
	if total == 0 {
		return []*Invocation{}, 0, nil
	}

	offset := (page - 1) * perPage
	rows, err := s.pool.Query(
		ctx, `
		SELECT `+selectCols+`
		FROM invocations
		WHERE tenant_id=$1 AND agent_id=$2
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`,
		tenantID, agentID, perPage, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list invocations: %w", err)
	}
	defer rows.Close()

	var invs []*Invocation
	for rows.Next() {
		inv, scanErr := scanInvocation(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("scan invocation: %w", scanErr)
		}
		invs = append(invs, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list invocations: %w", err)
	}

	if invs == nil {
		invs = []*Invocation{}
	}
	return invs, total, nil
}

// ListFiltered implements Store.
func (s *PostgresStore) ListFiltered(ctx context.Context, tenantID string, filter ListFilter) ([]*Invocation, int, error) {
	limit := filter.Limit
	if limit < 1 {
		limit = 20
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	// Build the WHERE clause dynamically to support any combination of filters.
	// $1 is always tenant_id; subsequent placeholders are assigned as filters are
	// added. n tracks the next available placeholder index.
	args := []any{tenantID}
	clauses := []string{"tenant_id=$1"}
	n := 2

	if filter.AgentID != "" {
		clauses = append(clauses, fmt.Sprintf("agent_id=$%d", n))
		args = append(args, filter.AgentID)
		n++
	}
	if filter.Status != nil {
		clauses = append(clauses, fmt.Sprintf("status=$%d", n))
		args = append(args, string(*filter.Status))
		n++
	}
	if filter.Mode != nil {
		clauses = append(clauses, fmt.Sprintf("mode=$%d", n))
		args = append(args, string(*filter.Mode))
		n++
	}
	if filter.ErrorClass != nil {
		clauses = append(clauses, fmt.Sprintf("error_class=$%d", n))
		args = append(args, string(*filter.ErrorClass))
		n++
	}
	if filter.Model != "" {
		clauses = append(clauses, fmt.Sprintf("model=$%d", n))
		args = append(args, filter.Model)
		n++
	}
	if filter.SettlementStatus != nil {
		clauses = append(clauses, fmt.Sprintf("settlement_status=$%d", n))
		args = append(args, string(*filter.SettlementStatus))
		n++
	}
	if filter.Since != nil {
		clauses = append(clauses, fmt.Sprintf("created_at>=$%d", n))
		args = append(args, *filter.Since)
		n++
	}
	if filter.Until != nil {
		clauses = append(clauses, fmt.Sprintf("created_at<=$%d", n))
		args = append(args, *filter.Until)
		n++
	}

	where := "WHERE " + strings.Join(clauses, " AND ")

	var total int
	if err := s.pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM invocations "+where,
		args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count invocations filtered: %w", err)
	}
	if total == 0 {
		return []*Invocation{}, 0, nil
	}

	listArgs := append(args, limit, offset) //nolint:gocritic // intentional slice extension for pagination args
	rows, err := s.pool.Query(
		ctx,
		"SELECT "+selectCols+" FROM invocations "+where+
			fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", n, n+1),
		listArgs...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list invocations filtered: %w", err)
	}
	defer rows.Close()

	var invs []*Invocation
	for rows.Next() {
		inv, scanErr := scanInvocation(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("scan invocation: %w", scanErr)
		}
		invs = append(invs, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list invocations filtered: %w", err)
	}
	if invs == nil {
		invs = []*Invocation{}
	}
	return invs, total, nil
}

// Aggregate implements Store. Tenant scoping and the created_at range are
// filtered in SQL; the bucketing and percentile math run through the shared
// aggregateRows helper, so this backend produces results identical to
// MemoryStore. Only the columns the aggregator needs are selected.
func (s *PostgresStore) Aggregate(ctx context.Context, tenantID string, q AggregateQuery) ([]AggregateGroup, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT agent_id, created_at, status, error_class, latency_ms, ttft_ms
		   FROM invocations
		  WHERE tenant_id=$1 AND created_at>=$2 AND created_at<=$3`,
		tenantID, q.Since, q.Until,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate invocations: %w", err)
	}
	defer rows.Close()

	var recs []*Invocation
	for rows.Next() {
		var (
			inv        Invocation
			status     string
			errorClass *string
			latencyMS  *int64
			ttftMS     *int64
		)
		if err := rows.Scan(
			&inv.AgentID, &inv.CreatedAt, &status, &errorClass, &latencyMS, &ttftMS,
		); err != nil {
			return nil, fmt.Errorf("scan aggregate row: %w", err)
		}
		inv.Status = Status(status)
		if errorClass != nil {
			ec := ErrorClass(*errorClass)
			inv.ErrorClass = &ec
		}
		inv.LatencyMS = latencyMS
		inv.TTFTMS = ttftMS
		recs = append(recs, &inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate invocations: %w", err)
	}

	return aggregateRows(recs, q.Bucket), nil
}

// Update implements Store. It fetches the current record first so that fields
// absent from UpdateFields keep their stored values.
func (s *PostgresStore) Update(ctx context.Context, tenantID, id string, fields UpdateFields) (*Invocation, error) {
	// Read-modify-write under a row lock so concurrent Updates to the same
	// invocation (e.g. marking it succeeded while another sets latency) can't
	// interleave between the read and the write. The FOR UPDATE lock is held
	// until the transaction commits.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("update invocation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	current, err := scanInvocation(tx.QueryRow(
		ctx,
		`SELECT `+selectCols+` FROM invocations WHERE id=$1 AND tenant_id=$2 FOR UPDATE`,
		id, tenantID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("update invocation: load: %w", err)
	}

	status := current.Status
	if fields.Status != nil {
		status = *fields.Status
	}
	completedAt := current.CompletedAt
	if fields.CompletedAt != nil {
		t := *fields.CompletedAt
		completedAt = &t
	}
	upstreamStatus := current.UpstreamStatus
	if fields.UpstreamStatus != nil {
		v := *fields.UpstreamStatus
		upstreamStatus = &v
	}
	latencyMS := current.LatencyMS
	if fields.LatencyMS != nil {
		v := *fields.LatencyMS
		latencyMS = &v
	}
	reqSize := current.RequestSize
	if fields.RequestSize != nil {
		v := *fields.RequestSize
		reqSize = &v
	}
	respSize := current.ResponseSize
	if fields.ResponseSize != nil {
		v := *fields.ResponseSize
		respSize = &v
	}
	reqBody := current.RequestBody
	if fields.RequestBody != nil {
		reqBody = *fields.RequestBody
	}
	respBody := current.ResponseBody
	if fields.ResponseBody != nil {
		respBody = *fields.ResponseBody
	}
	ttftMS := current.TTFTMS
	if fields.TTFTMS != nil {
		v := *fields.TTFTMS
		ttftMS = &v
	}
	errorClass := current.ErrorClass
	if fields.ErrorClass != nil {
		ec := *fields.ErrorClass
		errorClass = &ec
	}
	model := current.Model
	if fields.Model != nil {
		m := *fields.Model
		model = &m
	}
	mcpMethod := current.MCPMethod
	if fields.MCPMethod != nil {
		mm := *fields.MCPMethod
		mcpMethod = &mm
	}
	mcpTool := current.MCPTool
	if fields.MCPTool != nil {
		mt := *fields.MCPTool
		mcpTool = &mt
	}
	paymentNetwork := current.PaymentNetwork
	if fields.PaymentNetwork != nil {
		pn := *fields.PaymentNetwork
		paymentNetwork = &pn
	}
	paymentAsset := current.PaymentAsset
	if fields.PaymentAsset != nil {
		pa := *fields.PaymentAsset
		paymentAsset = &pa
	}
	paymentAmount := current.PaymentAmount
	if fields.PaymentAmount != nil {
		pa := *fields.PaymentAmount
		paymentAmount = &pa
	}
	paymentPayer := current.PaymentPayer
	if fields.PaymentPayer != nil {
		pp := *fields.PaymentPayer
		paymentPayer = &pp
	}
	paymentNonce := current.PaymentNonce
	if fields.PaymentNonce != nil {
		pn := *fields.PaymentNonce
		paymentNonce = &pn
	}
	settlementStatus := current.SettlementStatus
	if fields.SettlementStatus != nil {
		ss := *fields.SettlementStatus
		settlementStatus = &ss
	}
	settlementTxHash := current.SettlementTxHash
	if fields.SettlementTxHash != nil {
		th := *fields.SettlementTxHash
		settlementTxHash = &th
	}
	settledAmount := current.SettledAmount
	if fields.SettledAmount != nil {
		sa := *fields.SettledAmount
		settledAmount = &sa
	}
	operatorAmount := current.OperatorAmount
	if fields.OperatorAmount != nil {
		oa := *fields.OperatorAmount
		operatorAmount = &oa
	}
	facilitatorFee := current.FacilitatorFee
	if fields.FacilitatorFee != nil {
		ff := *fields.FacilitatorFee
		facilitatorFee = &ff
	}
	settlementAttempts := current.SettlementAttempts
	if fields.SettlementAttempts != nil {
		sa := *fields.SettlementAttempts
		settlementAttempts = &sa
	}
	settlementReason := current.SettlementReason
	if fields.SettlementReason != nil {
		sr := *fields.SettlementReason
		settlementReason = &sr
	}
	settledAt := current.SettledAt
	if fields.SettledAt != nil {
		t := *fields.SettledAt
		settledAt = &t
	}

	var errorClassStr *string
	if errorClass != nil {
		ec := string(*errorClass)
		errorClassStr = &ec
	}

	var settlementStatusStr *string
	if settlementStatus != nil {
		ss := string(*settlementStatus)
		settlementStatusStr = &ss
	}

	updated, err := scanInvocation(tx.QueryRow(
		ctx, `
		UPDATE invocations
		SET status=$3, completed_at=$4, upstream_status=$5, latency_ms=$6,
		    req_size=$7, resp_size=$8, req_body=$9, resp_body=$10,
		    ttft_ms=$11, error_class=$12, model=$13, mcp_method=$14, mcp_tool=$15,
		    payment_network=$16, payment_asset=$17, payment_amount=$18, payment_payer=$19, payment_nonce=$20,
		    settlement_status=$21, settlement_tx_hash=$22, settled_amount=$23, operator_amount=$24,
		    facilitator_fee=$25, settlement_attempts=$26, settlement_reason=$27, settled_at=$28,
		    updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2
		RETURNING `+selectCols,
		id, tenantID,
		string(status), completedAt, upstreamStatus, latencyMS,
		reqSize, respSize, reqBody, respBody,
		ttftMS, errorClassStr, model, mcpMethod, mcpTool,
		paymentNetwork, paymentAsset, paymentAmount, paymentPayer, paymentNonce,
		settlementStatusStr, settlementTxHash, settledAmount, operatorAmount,
		facilitatorFee, settlementAttempts, settlementReason, settledAt,
	))
	if err != nil {
		// The row was located and locked above, so this should not be ErrNoRows.
		return nil, fmt.Errorf("update invocation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("update invocation: commit: %w", err)
	}
	return updated, nil
}
