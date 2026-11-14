package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// 质量事件
// ---------------------------------------------------------------------------

// CreateQualityEvent records a problem with one truck. The event is the anchor
// every downstream investigation and isolation order hangs from. Repeated
// submissions with the same event_no return the stored event, so a retried
// report never creates a second incident.
func (a *App) CreateQualityEvent(ctx context.Context, actor User, in QualityEventInput) (QualityEvent, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return QualityEvent{}, err
	}
	if in.EventNo == "" {
		return QualityEvent{}, invalid("event_no（事件号）必填，用于幂等重试")
	}
	if in.Kind != "composition" && in.Kind != "risk_range" {
		return QualityEvent{}, invalid("kind 必须为 composition（具体成分）或 risk_range（风险范围）")
	}
	if in.Param != "metal_ppm" && in.Param != "impurity_ppm" && in.Param != "moisture_ppm" {
		return QualityEvent{}, invalid("param 必须为 metal_ppm / impurity_ppm / moisture_ppm")
	}
	if in.MinPPM < 0 || in.MaxPPM > 1_000_000 || in.MinPPM > in.MaxPPM {
		return QualityEvent{}, invalid("ppm 范围必须满足 0 <= min <= max <= 1000000")
	}
	if in.Basis == "" {
		in.Basis = "manual"
	}
	switch in.Basis {
	case "adopted_assay", "recheck", "customer_complaint", "manual":
	default:
		return QualityEvent{}, invalid("basis 取值不合法")
	}
	var outID int64
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if in.TripID <= 0 && in.TripKey != "" {
			if err := tx.QueryRow(ctx, `select id from rt.trips where batch_no=$1`, in.TripKey).Scan(&in.TripID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return notFound("trip_key %q", in.TripKey)
				}
				return err
			}
		}
		if in.TripID <= 0 {
			return invalid("trip_id 或 trip_key（批次号）必须提供一个")
		}
		var tripExists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from rt.trips where id=$1)`, in.TripID).Scan(&tripExists); err != nil {
			return err
		}
		if !tripExists {
			return notFound("trip %d", in.TripID)
		}
		var existing int64
		err := tx.QueryRow(ctx, `select id from rt.quality_events where event_no=$1`, in.EventNo).Scan(&existing)
		if err == nil {
			outID = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		minPPM, maxPPM := in.MinPPM, in.MaxPPM
		if in.Kind == "composition" {
			maxPPM = minPPM
		}
		err = tx.QueryRow(ctx, `insert into rt.quality_events(event_no, kind, trip_id, param, min_ppm, max_ppm,
			basis, note, reported_by) values($1,$2,$3,$4,$5,$6,$7,$8,$9) returning id`,
			in.EventNo, in.Kind, in.TripID, in.Param, minPPM, maxPPM, in.Basis, in.Note, actor.ID).Scan(&outID)
		if err != nil {
			return conflict("质量事件 %q 已存在", in.EventNo)
		}
		return insertAudit(ctx, tx, actor, "quality.event.create", "quality_event", outID, map[string]any{
			"trip_id": in.TripID, "param": in.Param, "min_ppm": minPPM, "max_ppm": maxPPM})
	})
	if err != nil {
		return QualityEvent{}, err
	}
	return a.GetQualityEvent(ctx, outID)
}

func (a *App) GetQualityEvent(ctx context.Context, id int64) (QualityEvent, error) {
	var e QualityEvent
	var latestID *int64
	var latestSeq int
	err := a.DB.Pool.QueryRow(ctx, `select e.id, e.event_no, e.kind, e.trip_id, coalesce(t.batch_no,''), e.param,
		e.min_ppm, e.max_ppm, e.basis, e.note, e.reported_by,
		to_char(e.reported_at,'YYYY-MM-DD"T"HH24:MI:SSOF'), e.status,
		(select id from rt.quality_investigations qi where qi.event_id=e.id order by seq desc limit 1),
		coalesce((select max(seq) from rt.quality_investigations qi where qi.event_id=e.id),0)
		from rt.quality_events e left join rt.trips t on t.id=e.trip_id where e.id=$1`, id).
		Scan(&e.ID, &e.EventNo, &e.Kind, &e.TripID, &e.BatchNo, &e.Param, &e.MinPPM, &e.MaxPPM,
			&e.Basis, &e.Note, &e.ReportedBy, &e.ReportedAt, &e.Status, &latestID, &latestSeq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return e, notFound("quality event %d", id)
		}
		return e, err
	}
	e.LatestInvestigationID = latestID
	e.LatestSeq = latestSeq
	return e, nil
}

func (a *App) ListQualityEvents(ctx context.Context) ([]QualityEvent, error) {
	rows, err := a.DB.Pool.Query(ctx, `select id from rt.quality_events order by id desc`)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []QualityEvent{}
	for _, id := range ids {
		e, err := a.GetQualityEvent(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 排查快照
// ---------------------------------------------------------------------------

// Investigation is one immutable recall snapshot for an event.
type Investigation struct {
	ID                     int64         `json:"id"`
	EventID                int64         `json:"event_id"`
	EventNo                string        `json:"event_no"`
	TripID                 int64         `json:"trip_id"`
	BatchNo                string        `json:"batch_no"`
	Seq                    int           `json:"seq"`
	StockEpoch             int64         `json:"stock_epoch"`
	Param                  string        `json:"param"`
	LowerPPM               int64         `json:"lower_ppm"`
	UpperPPM               int64         `json:"upper_ppm"`
	Exact                  bool          `json:"exact"`
	OnHandG                int64         `json:"on_hand_g"`
	OnHandLowerG           int64         `json:"on_hand_lower_g"`
	OnHandComponentMg      int64         `json:"on_hand_component_mg"`
	ConsumedG              int64         `json:"consumed_g"`
	ConsumedLowerG         int64         `json:"consumed_lower_g"`
	ConsumedComponentMg    int64         `json:"consumed_component_mg"`
	ProductionG            int64         `json:"production_g"`
	ProductionLowerG       int64         `json:"production_lower_g"`
	ProductionComponentMg  int64         `json:"production_component_mg"`
	ShippedG               int64         `json:"shipped_g"`
	ShippedLowerG          int64         `json:"shipped_lower_g"`
	ShippedComponentMg     int64         `json:"shipped_component_mg"`
	LossG                  int64         `json:"loss_g"`
	LossLowerG             int64         `json:"loss_lower_g"`
	LossComponentMg       int64         `json:"loss_component_mg"`
	UnrecoverableG         int64         `json:"unrecoverable_g"`
	SourceIntakeG          int64         `json:"source_intake_g"`
	CheckedG               int64         `json:"checked_g"`
	LedgerG                int64         `json:"ledger_g"`
	GapG                   int64         `json:"gap_g"`
	LowerG                 int64         `json:"lower_g"`
	UpperG                 int64         `json:"upper_g"`
	LowerComponentMg       int64         `json:"lower_component_mg"`
	UpperComponentMg       int64         `json:"upper_component_mg"`
	CapViolations          int           `json:"cap_violations"`
	NodeCount              int           `json:"node_count"`
	Nodes                  []RecallNode  `json:"nodes"`
	Reconcile              ReconcileView `json:"reconcile"`
	Stale                  bool          `json:"stale"`
	CreatedBy              int64         `json:"created_by"`
	CreatedAt              string        `json:"created_at"`
}

// ReconcileView presents the disposal totals against the source truck's
// explainable range, so nobody has to add up numbers by hand.
type ReconcileView struct {
	SourceIntakeG int64 `json:"source_intake_g"`
	OnHandG       int64 `json:"on_hand_g"`
	ConsumedG     int64 `json:"consumed_g"`
	ProductionG   int64 `json:"production_g"`
	ShippedG      int64 `json:"shipped_g"`
	LossG         int64 `json:"loss_g"`
	CheckedG      int64 `json:"checked_g"`
	GapG          int64 `json:"gap_g"`
	Balanced      bool  `json:"balanced"`
	Capped        bool  `json:"nodes_within_receipts"`
}

type investigationDetail struct {
	Nodes []RecallNode `json:"nodes"`
}

// Investigate runs the propagation for an event and persists the snapshot.
// A new run always produces a new seq; the previous snapshot stays readable so
// an approval can prove which version it was based on.
func (a *App) Investigate(ctx context.Context, actor User, eventID int64) (Investigation, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return Investigation{}, err
	}
	var invID int64
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockStockWorld(ctx, tx); err != nil {
			return err
		}
		ev, err := a.GetQualityEvent(ctx, eventID)
		if err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRow(ctx, `select coalesce(max(seq),0)+1 from rt.quality_investigations where event_id=$1`, eventID).
			Scan(&seq); err != nil {
			return err
		}
		an, err := analyzeRecall(ctx, tx, ev.TripID, ev.Param, ev.MinPPM, ev.MaxPPM)
		if err != nil {
			return err
		}
		epoch, err := currentEpoch(ctx, tx)
		if err != nil {
			return err
		}
		detail, err := json.Marshal(map[string]any{"nodes": an.Nodes})
		if err != nil {
			return err
		}
		unrecoverable := an.ProductionG + an.ShippedG + an.ConsumedG
		err = tx.QueryRow(ctx, `insert into rt.quality_investigations(event_id, seq, stock_epoch, param,
			lower_ppm, upper_ppm, exact, on_hand_g, on_hand_component_mg, consumed_g, consumed_component_mg,
			production_g, production_component_mg, shipped_g, shipped_component_mg, loss_g, loss_component_mg,
			unrecoverable_g, source_intake_g, checked_g, ledger_g, gap_g, lower_g, upper_g,
			lower_component_mg, upper_component_mg, cap_violations, node_count, detail, created_by,
			on_hand_lower_g, consumed_lower_g, production_lower_g, shipped_lower_g, loss_lower_g)
			values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,
				$31,$32,$33,$34,$35)
			returning id`,
			eventID, seq, epoch, an.Param, an.LowerPPM, an.UpperPPM, an.Exact,
			an.OnHandG, an.OnHandComponentMg, an.ConsumedG, an.ConsumedComponentMg,
			an.ProductionG, an.ProductionComponentMg, an.ShippedG, an.ShippedComponentMg,
			an.LossG, an.LossComponentMg, unrecoverable, an.SourceIntakeG, an.CheckedG, an.LedgerG,
			an.GapG, an.LowerG, an.UpperG, an.LowerComponentMg, an.UpperComponentMg,
			an.CapViolations, len(an.Nodes), detail, actor.ID,
			an.OnHandLowerG, an.ConsumedLowerG, an.ProductionLowerG, an.ShippedLowerG, an.LossLowerG).Scan(&invID)
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, actor, "quality.investigate", "quality_investigation", invID, map[string]any{
			"event_id": eventID, "seq": seq, "upper_g": an.UpperG, "on_hand_g": an.OnHandG,
			"production_g": an.ProductionG, "shipped_g": an.ShippedG})
	})
	if err != nil {
		return Investigation{}, err
	}
	return a.GetInvestigation(ctx, invID)
}

func (a *App) GetInvestigation(ctx context.Context, id int64) (Investigation, error) {
	var inv Investigation
	var detail []byte
	err := a.DB.Pool.QueryRow(ctx, `select qi.id, qi.event_id, e.event_no, e.trip_id, coalesce(t.batch_no,''),
		qi.seq, qi.stock_epoch, qi.param, qi.lower_ppm, qi.upper_ppm, qi.exact,
		qi.on_hand_g, qi.on_hand_component_mg, qi.consumed_g, qi.consumed_component_mg,
		qi.production_g, qi.production_component_mg, qi.shipped_g, qi.shipped_component_mg,
		qi.loss_g, qi.loss_component_mg, qi.unrecoverable_g, qi.source_intake_g, qi.checked_g, qi.ledger_g,
		qi.gap_g, qi.lower_g, qi.upper_g, qi.lower_component_mg, qi.upper_component_mg,
		qi.cap_violations, qi.node_count, qi.detail, qi.created_by,
		to_char(qi.created_at,'YYYY-MM-DD"T"HH24:MI:SSOF'),
		qi.on_hand_lower_g, qi.consumed_lower_g, qi.production_lower_g, qi.shipped_lower_g, qi.loss_lower_g
		from rt.quality_investigations qi join rt.quality_events e on e.id=qi.event_id
		left join rt.trips t on t.id=e.trip_id where qi.id=$1`, id).
		Scan(&inv.ID, &inv.EventID, &inv.EventNo, &inv.TripID, &inv.BatchNo, &inv.Seq, &inv.StockEpoch,
			&inv.Param, &inv.LowerPPM, &inv.UpperPPM, &inv.Exact,
			&inv.OnHandG, &inv.OnHandComponentMg, &inv.ConsumedG, &inv.ConsumedComponentMg,
			&inv.ProductionG, &inv.ProductionComponentMg, &inv.ShippedG, &inv.ShippedComponentMg,
			&inv.LossG, &inv.LossComponentMg, &inv.UnrecoverableG, &inv.SourceIntakeG, &inv.CheckedG, &inv.LedgerG,
			&inv.GapG, &inv.LowerG, &inv.UpperG, &inv.LowerComponentMg, &inv.UpperComponentMg,
			&inv.CapViolations, &inv.NodeCount, &detail, &inv.CreatedBy, &inv.CreatedAt,
			&inv.OnHandLowerG, &inv.ConsumedLowerG, &inv.ProductionLowerG, &inv.ShippedLowerG, &inv.LossLowerG)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return inv, notFound("investigation %d", id)
		}
		return inv, err
	}
	var d investigationDetail
	_ = json.Unmarshal(detail, &d)
	if d.Nodes == nil {
		d.Nodes = []RecallNode{}
	}
	inv.Nodes = d.Nodes
	epoch, _ := a.epochOf(ctx)
	inv.Stale = epoch != inv.StockEpoch
	inv.Reconcile = ReconcileView{
		SourceIntakeG: inv.SourceIntakeG, OnHandG: inv.OnHandG, ConsumedG: inv.ConsumedG,
		ProductionG: inv.ProductionG, ShippedG: inv.ShippedG, LossG: inv.LossG,
		CheckedG: inv.CheckedG, GapG: inv.GapG,
		Balanced: inv.GapG == 0 && inv.LedgerG >= inv.OnHandG,
		Capped:   inv.CapViolations == 0,
	}
	return inv, nil
}

func (a *App) ListInvestigations(ctx context.Context, eventID int64) ([]Investigation, error) {
	sql := `select id from rt.quality_investigations`
	args := []any{}
	if eventID > 0 {
		sql += ` where event_id=$1`
		args = append(args, eventID)
	}
	sql += ` order by seq`
	rows, err := a.DB.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Investigation{}
	for _, id := range ids {
		inv, err := a.GetInvestigation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

func (a *App) epochOf(ctx context.Context) (int64, error) {
	return currentEpoch(ctx, a.DB.Pool)
}

// ---------------------------------------------------------------------------
// 隔离处置
// ---------------------------------------------------------------------------

type IsolationOrder struct {
	ID              int64          `json:"id"`
	OrderNo         string         `json:"order_no"`
	EventID         int64          `json:"event_id"`
	EventNo         string         `json:"event_no"`
	InvestigationID int64          `json:"investigation_id"`
	AppliedEpoch    int64          `json:"applied_epoch"`
	ApprovedEpoch   *int64         `json:"approved_epoch"`
	Status          string         `json:"status"`
	ScopeNote       string         `json:"scope_note"`
	AppliedBy       int64          `json:"applied_by"`
	AppliedAt       string         `json:"applied_at"`
	ApprovedAt      *string        `json:"approved_at"`
	Note            string         `json:"note"`
	Stale           bool           `json:"stale"`
	Investigation   *Investigation `json:"investigation,omitempty"`
	Holds           []HoldView     `json:"holds"`
	Reviews         []ReviewView   `json:"reviews"`
	// 申请/批准前页面必须展示的“依赖当前存量及正在执行动作”。
	Depending      []HoldView   `json:"depending"`
	InFlight       []InFlight   `json:"in_flight"`
	RemainingHoldG int64        `json:"remaining_hold_g"`
}

// HoldView is one held lot with its current balance and event attribution.
type HoldView struct {
	ID          int64  `json:"id"`
	EventID     int64  `json:"event_id"`
	EventNo     string `json:"event_no"`
	LotID       int64  `json:"lot_id"`
	LotKey      string `json:"lot_key"`
	PileName    string `json:"pile_name"`
	HitWeightG  int64  `json:"hit_weight_g"`
	HitLowerG   int64  `json:"hit_lower_g"`
	HitUpperG   int64  `json:"hit_upper_g"`
	Basis       string `json:"basis"`
	CurrentG    int64  `json:"current_g"`
	Released    bool   `json:"released"`
	Reviewed    bool   `json:"reviewed"`
}

// InFlight is an action already applied downstream that cannot be rolled back.
type InFlight struct {
	MovementID  int64  `json:"movement_id"`
	ExternalRef string `json:"external_ref"`
	Kind        string `json:"kind"`
	OccurredAt  string `json:"occurred_at"`
	LotKey      string `json:"lot_key"`
	PileKind    string `json:"pile_kind"`
	WeightG     int64  `json:"weight_g"`
}

type ReviewView struct {
	ID        int64   `json:"id"`
	Coverage  string  `json:"coverage"`
	ScopeLots []int64 `json:"scope_lot_ids"`
	ScopeNote string  `json:"scope_note"`
	ReviewsAt string  `json:"reviewed_at"`
}

type ApplyIsolationInput struct {
	OrderNo    string `json:"order_no"`
	EventID    int64  `json:"event_id"`
	InvestigationID int64 `json:"investigation_id"`
	ScopeNote  string `json:"scope_note"`
	Note       string `json:"note"`
}

// HoldsForEvent reports which lots are held by any active order of an event.
func holdsForEvent(ctx context.Context, q querier, eventID int64) ([]HoldView, error) {
	rows, err := q.Query(ctx, `select h.id, h.event_id, e.event_no, h.lot_id, h.lot_key, h.pile_name,
		h.hit_weight_g, h.hit_lower_g, h.hit_upper_g, h.basis, h.released, l.weight_g
		from rt.lot_holds h join rt.quality_events e on e.id=h.event_id
		join rt.inventory_lots l on l.id=h.lot_id
		where h.event_id=$1 order by h.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HoldView{}
	for rows.Next() {
		var h HoldView
		if err := rows.Scan(&h.ID, &h.EventID, &h.EventNo, &h.LotID, &h.LotKey, &h.PileName,
			&h.HitWeightG, &h.HitLowerG, &h.HitUpperG, &h.Basis, &h.Released, &h.CurrentG); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ApplyIsolation prepares (but does not activate) an isolation order. Nothing
// is held yet; the approver sees exactly which current stock and in-flight
// actions the request depends on. Applying does not need the epoch to be
// unchanged; approving does.
func (a *App) ApplyIsolation(ctx context.Context, actor User, in ApplyIsolationInput) (IsolationOrder, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return IsolationOrder{}, err
	}
	if in.OrderNo == "" {
		return IsolationOrder{}, invalid("order_no（处置单号）必填，用于幂等重试")
	}
	var outID int64
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockStockWorld(ctx, tx); err != nil {
			return err
		}
		var existing int64
		err := tx.QueryRow(ctx, `select id from rt.isolation_orders where order_no=$1`, in.OrderNo).Scan(&existing)
		if err == nil {
			outID = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		inv, err := a.GetInvestigation(ctx, in.InvestigationID)
		if err != nil {
			return err
		}
		if inv.EventID != in.EventID {
			return invalid("排查快照 %d 不属于事件 %d", in.InvestigationID, in.EventID)
		}
		ev, err := a.GetQualityEvent(ctx, in.EventID)
		if err != nil {
			return err
		}
		if ev.Status != "open" {
			return blocked("事件 %s 已关闭，不能再申请隔离", ev.EventNo)
		}
		holds, err := holdsForEvent(ctx, tx, in.EventID)
		if err != nil {
			return err
		}
		for _, h := range holds {
			if !h.Released {
				return conflict("事件 %s 已有生效中的隔离持有（批次 %s），不重复申请", ev.EventNo, h.LotKey)
			}
		}
		epoch, err := currentEpoch(ctx, tx)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `insert into rt.isolation_orders(order_no, event_id, investigation_id, applied_epoch,
			status, scope_note, applied_by, note) values($1,$2,$3,$4,'pending',$5,$6,$7) returning id`,
			in.OrderNo, in.EventID, in.InvestigationID, epoch, in.ScopeNote, actor.ID, in.Note).Scan(&outID)
		if err != nil {
			return conflict("处置单 %q 已存在", in.OrderNo)
		}
		return insertAudit(ctx, tx, actor, "isolation.apply", "isolation_order", outID, map[string]any{
			"event_id": in.EventID, "investigation_id": in.InvestigationID, "epoch": epoch})
	})
	if err != nil {
		return IsolationOrder{}, err
	}
	return a.GetIsolationOrder(ctx, outID)
}

// ApproveIsolation activates the holds. It refuses an investigation computed
// before a later transfer, because the affected set and quantities would no
// longer describe the current stock.
func (a *App) ApproveIsolation(ctx context.Context, actor User, orderID int64) (IsolationOrder, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return IsolationOrder{}, err
	}
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockStockWorld(ctx, tx); err != nil {
			return err
		}
		var eventID, invID int64
		var status, eventNo, eventStatus string
		err := tx.QueryRow(ctx, `select o.event_id, o.investigation_id, o.status, e.event_no, e.status
			from rt.isolation_orders o join rt.quality_events e on e.id=o.event_id
			where o.id=$1 for update of o`, orderID).
			Scan(&eventID, &invID, &status, &eventNo, &eventStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("isolation order %d", orderID)
			}
			return err
		}
		if status != "pending" {
			return conflict("处置单状态 %s，不能批准", status)
		}
		if eventStatus != "open" {
			return blocked("事件 %s 已关闭", eventNo)
		}
		inv, err := a.GetInvestigation(ctx, invID)
		if err != nil {
			return err
		}
		epoch, err := currentEpoch(ctx, tx)
		if err != nil {
			return err
		}
		if epoch != inv.StockEpoch {
			return conflict("排查快照基于库存序号 %d，当前为 %d；期间发生了新的转移，请重新排查后再批准（旧结论不可直接批准）",
				inv.StockEpoch, epoch)
		}
		// Hold every on-hand node the investigation attributed to this event.
		held := 0
		for _, n := range inv.Nodes {
			if n.Bucket != "on_hand" || n.UpperG <= 0 {
				continue
			}
			basis := "uniform_share"
			if n.Basis == "bounded" {
				basis = "unknown"
			}
			// unique (event_id, lot_id): an event retry or a second order never
			// adds the same lot's quantity twice. A row released by a previous
			// review is re-opened under this order rather than duplicated, so a
			// re-investigation can hold the lot again without inflating totals.
			if _, err := tx.Exec(ctx, `insert into rt.lot_holds(order_id, event_id, lot_id, lot_key, pile_name,
				hit_weight_g, hit_lower_g, hit_upper_g, basis)
				values($1,$2,$3,$4,$5,$6,$7,$8,$9)
				on conflict (event_id, lot_id) do update
				  set order_id=excluded.order_id, hit_weight_g=excluded.hit_weight_g,
				      hit_lower_g=excluded.hit_lower_g, hit_upper_g=excluded.hit_upper_g,
				      basis=excluded.basis, lot_key=excluded.lot_key, pile_name=excluded.pile_name,
				      released=false, released_at=null, release_review_id=null
				  where rt.lot_holds.released = true`,
				orderID, eventID, n.LotID, n.LotKey, n.PileName, n.UpperG, n.LowerG, n.UpperG, basis); err != nil {
				return err
			}
			held++
		}
		if held == 0 {
			return blocked("排查快照没有命中的在库批次，无法批准隔离")
		}
		if _, err := tx.Exec(ctx, `update rt.isolation_orders set status='active', approved_by=$2, approved_at=now(),
			approved_epoch=$3 where id=$1`, orderID, actor.ID, epoch); err != nil {
			return err
		}
		return insertAudit(ctx, tx, actor, "isolation.approve", "isolation_order", orderID, map[string]any{
			"event_id": eventID, "held_lots": held, "epoch": epoch})
	})
	if err != nil {
		return IsolationOrder{}, err
	}
	return a.GetIsolationOrder(ctx, orderID)
}

// ReviewIsolation records a scope review. A review covering only part of the
// holds leaves the rest blocked. The order only closes when, for every held
// lot on this event, a review covers it — and when that lot no longer has any
// unreviewed hold from any other event either.
func (a *App) ReviewIsolation(ctx context.Context, actor User, orderID int64, scopeLotIDs []int64, scopeNote string) (IsolationOrder, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return IsolationOrder{}, err
	}
	err := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockStockWorld(ctx, tx); err != nil {
			return err
		}
		var eventID int64
		var status string
		err := tx.QueryRow(ctx, `select o.event_id, o.status from rt.isolation_orders o where o.id=$1 for update of o`, orderID).
			Scan(&eventID, &status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("isolation order %d", orderID)
			}
			return err
		}
		if status != "active" {
			return conflict("处置单状态 %s，不能复核", status)
		}
		// Validate the scope against this order's own holds.
		rows, err := tx.Query(ctx, `select lot_id from rt.lot_holds where order_id=$1 and released=false order by lot_id`, orderID)
		if err != nil {
			return err
		}
		own := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			own[id] = true
		}
		rows.Close()
		if len(own) == 0 {
			return conflict("处置单没有需要复核的持有")
		}
		cover := map[int64]bool{}
		if scopeLotIDs == nil {
			scopeLotIDs = []int64{}
		}
		if len(scopeLotIDs) == 0 {
			for id := range own {
				cover[id] = true
			}
		} else {
			for _, id := range scopeLotIDs {
				if !own[id] {
					return invalid("批次 %d 不在本处置单的持有范围内", id)
				}
				cover[id] = true
			}
		}
		coverage := "partial"
		if len(cover) == len(own) {
			coverage = "full"
		}
		var reviewID int64
		if err := tx.QueryRow(ctx, `insert into rt.isolation_reviews(order_id, event_id, scope_lot_ids, scope_note, coverage, reviewed_by)
			values($1,$2,$3,$4,$5,$6) returning id`, orderID, eventID, scopeLotIDs, scopeNote, coverage, actor.ID).
			Scan(&reviewID); err != nil {
			return err
		}
		for id := range cover {
			if _, err := tx.Exec(ctx, `update rt.lot_holds set released=true, released_at=now(), release_review_id=$3
				where order_id=$1 and lot_id=$2 and released=false`, orderID, id, reviewID); err != nil {
				return err
			}
		}
		// Close the order when every own hold is released and no other event still
		// holds the same lots through this order's scope.
		var remaining int
		if err := tx.QueryRow(ctx, `select count(*) from rt.lot_holds where event_id=$1 and released=false`, eventID).
			Scan(&remaining); err != nil {
		}
		if remaining == 0 {
			if _, err := tx.Exec(ctx, `update rt.isolation_orders set status='closed', closed_by=$2, closed_at=now() where id=$1`,
				orderID, actor.ID); err != nil {
				return err
			}
		}
		return insertAudit(ctx, tx, actor, "isolation.review", "isolation_order", orderID, map[string]any{
			"coverage": coverage, "scope_lots": len(cover), "remaining_event_holds": remaining})
	})
	if err != nil {
		return IsolationOrder{}, err
	}
	return a.GetIsolationOrder(ctx, orderID)
}

// CloseEvent closes the incident once no hold remains active for it.
func (a *App) CloseEvent(ctx context.Context, actor User, eventID int64) (QualityEvent, error) {
	if err := RequireRole(actor, "quality"); err != nil {
		return QualityEvent{}, err
	}
	var remaining int
	if err := a.DB.Pool.QueryRow(ctx, `select count(*) from rt.lot_holds where event_id=$1 and released=false`, eventID).
		Scan(&remaining); err != nil {
		return QualityEvent{}, err
	}
	if remaining > 0 {
		return QualityEvent{}, blocked("事件仍有 %d 个批次处于阻断状态，不能关闭", remaining)
	}
	_, err := a.DB.Pool.Exec(ctx, `update rt.quality_events set status='closed' where id=$1`, eventID)
	if err != nil {
		return QualityEvent{}, err
	}
	return a.GetQualityEvent(ctx, eventID)
}

func (a *App) GetIsolationOrder(ctx context.Context, id int64) (IsolationOrder, error) {
	var o IsolationOrder
	var approvedEpoch *int64
	var approvedAt *string
	err := a.DB.Pool.QueryRow(ctx, `select o.id, o.order_no, o.event_id, e.event_no, o.investigation_id,
		o.applied_epoch, o.approved_epoch, o.status, o.scope_note, o.applied_by,
		to_char(o.applied_at,'YYYY-MM-DD"T"HH24:MI:SSOF'), to_char(o.approved_at,'YYYY-MM-DD"T"HH24:MI:SSOF'),
		o.note
		from rt.isolation_orders o join rt.quality_events e on e.id=o.event_id where o.id=$1`, id).
		Scan(&o.ID, &o.OrderNo, &o.EventID, &o.EventNo, &o.InvestigationID, &o.AppliedEpoch, &approvedEpoch,
			&o.Status, &o.ScopeNote, &o.AppliedBy, &o.AppliedAt, &approvedAt, &o.Note)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return o, notFound("isolation order %d", id)
		}
		return o, err
	}
	o.ApprovedEpoch = approvedEpoch
	o.ApprovedAt = approvedAt
	inv, err := a.GetInvestigation(ctx, o.InvestigationID)
	if err != nil {
		return o, err
	}
	o.Investigation = &inv
	o.Stale = inv.Stale
	holds, err := holdsForOrder(ctx, a.DB.Pool, o.ID)
	if err != nil {
		return o, err
	}
	o.Holds = holds
	for _, h := range holds {
		if !h.Released {
			o.RemainingHoldG += h.HitUpperG
		}
	}
	// Reviews.
	rows, err := a.DB.Pool.Query(ctx, `select id, coverage, scope_lot_ids, scope_note,
		to_char(reviewed_at,'YYYY-MM-DD"T"HH24:MI:SSOF') from rt.isolation_reviews where order_id=$1 order by id`, o.ID)
	if err != nil {
		return o, err
	}
	o.Reviews = []ReviewView{}
	for rows.Next() {
		var r ReviewView
		if err := rows.Scan(&r.ID, &r.Coverage, &r.ScopeLots, &r.ScopeNote, &r.ReviewsAt); err != nil {
			rows.Close()
			return o, err
		}
		o.Reviews = append(o.Reviews, r)
	}
	rows.Close()
	// Depending: the stock this order depends on. Once active these are the
	// actual holds; while pending they are the would-be hits, derived from the
	// same investigation snapshot so the approver sees exactly what will be
	// blocked before approving.
	o.Depending = holds
	if len(o.Depending) == 0 && o.Status == "pending" {
		for _, n := range inv.Nodes {
			if n.Bucket != "on_hand" || n.UpperG <= 0 {
				continue
			}
			basis := "uniform_share"
			if n.Basis == "bounded" {
				basis = "unknown"
			}
			o.Depending = append(o.Depending, HoldView{
				EventID: o.EventID, EventNo: o.EventNo, LotID: n.LotID, LotKey: n.LotKey,
				PileName: n.PileName, HitWeightG: n.UpperG, HitLowerG: n.LowerG, HitUpperG: n.UpperG,
				Basis: basis, CurrentG: n.WeightG,
			})
		}
	}
	o.InFlight, err = inFlightForEvent(ctx, a.DB.Pool, o.EventID)
	if err != nil {
		return o, err
	}
	return o, nil
}

func holdsForOrder(ctx context.Context, q querier, orderID int64) ([]HoldView, error) {
	rows, err := q.Query(ctx, `select h.id, h.event_id, e.event_no, h.lot_id, h.lot_key, h.pile_name,
		h.hit_weight_g, h.hit_lower_g, h.hit_upper_g, h.basis, h.released, l.weight_g,
		exists(select 1 from rt.isolation_reviews r where r.order_id=h.order_id and h.released
		  and (cardinality(r.scope_lot_ids)=0 or h.lot_id = any(r.scope_lot_ids)))
		from rt.lot_holds h join rt.quality_events e on e.id=h.event_id
		join rt.inventory_lots l on l.id=h.lot_id where h.order_id=$1 order by h.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HoldView{}
	for rows.Next() {
		var h HoldView
		if err := rows.Scan(&h.ID, &h.EventID, &h.EventNo, &h.LotID, &h.LotKey, &h.PileName,
			&h.HitWeightG, &h.HitLowerG, &h.HitUpperG, &h.Basis, &h.Released, &h.CurrentG, &h.Reviewed); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// inFlightForEvent lists downstream actions from the event's truck whose
// resulting nodes are not on-hand stock (production/shipped/loss) or whose
// source has already been consumed. These cannot be reverted.
func inFlightForEvent(ctx context.Context, q querier, eventID int64) ([]InFlight, error) {
	var tripID int64
	if err := q.QueryRow(ctx, `select trip_id from rt.quality_events where id=$1`, eventID).Scan(&tripID); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `select distinct m.id, m.external_ref, m.kind,
		to_char(m.occurred_at,'YYYY-MM-DD"T"HH24:MI:SSOF'), l.lot_key, p.kind, l.weight_g
		from rt.movements m
		join rt.transfer_lines tl on tl.movement_id=m.id and tl.direction='in'
		join rt.inventory_lots l on l.id=tl.lot_id
		join rt.piles p on p.id=l.pile_id
		where p.kind in ('production','shipped','loss')
		  and exists(select 1 from rt.lot_origins o where o.lot_id=l.id and o.trip_id=$1 and o.weight_g > 0)
		order by m.id, l.lot_key, p.kind`, tripID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InFlight{}
	for rows.Next() {
		var f InFlight
		if err := rows.Scan(&f.MovementID, &f.ExternalRef, &f.Kind, &f.OccurredAt, &f.LotKey, &f.PileKind, &f.WeightG); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ActiveHoldsForLot returns unreleased holds across all events for one lot.
func (a *App) ActiveHoldsForLot(ctx context.Context, q querier, lotID int64) ([]HoldView, error) {
	rows, err := q.Query(ctx, `select h.id, h.event_id, e.event_no, h.lot_id, h.lot_key, h.pile_name,
		h.hit_weight_g, h.hit_lower_g, h.hit_upper_g, h.basis, h.released, l.weight_g
		from rt.lot_holds h join rt.quality_events e on e.id=h.event_id
		join rt.inventory_lots l on l.id=h.lot_id
		where h.lot_id=$1 and h.released=false order by h.id`, lotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HoldView{}
	for rows.Next() {
		var h HoldView
		if err := rows.Scan(&h.ID, &h.EventID, &h.EventNo, &h.LotID, &h.LotKey, &h.PileName,
			&h.HitWeightG, &h.HitLowerG, &h.HitUpperG, &h.Basis, &h.Released, &h.CurrentG); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListHoldsByEvent is used by the UI's incident board.
func (a *App) ListHoldsByEvent(ctx context.Context, eventID int64) ([]HoldView, error) {
	return holdsForEvent(ctx, a.DB.Pool, eventID)
}

// ListIsolationOrders lists all isolation orders, newest first.
func (a *App) ListIsolationOrders(ctx context.Context) ([]IsolationOrder, error) {
	rows, err := a.DB.Pool.Query(ctx, `select id from rt.isolation_orders order by id desc`)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []IsolationOrder{}
	for _, id := range ids {
		o, err := a.GetIsolationOrder(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
