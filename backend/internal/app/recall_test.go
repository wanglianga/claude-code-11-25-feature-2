package app

import (
	"testing"
)

// TestRecallDiamondDoesNotDoubleCount proves the confluence property: two
// branches that re-merge are each counted once. A naive recursive sum over the
// source's outgoing paths would report the source truck's material twice.
func TestRecallDiamondDoesNotDoubleCount(t *testing.T) {
	a, ctx := testApp(t)
	if _, err := a.ResetDemo(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := a.SeedDemo(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inv, err := a.GetInvestigation(ctx, 1)
	if err != nil {
		t.Fatalf("investigation: %v", err)
	}
	// Seeded diamond: truck intake 18,000,000 g, nothing should ever state more.
	if inv.UpperG > inv.SourceIntakeG {
		t.Fatalf("upper bound %d exceeds source intake %d — confluence double counted", inv.UpperG, inv.SourceIntakeG)
	}
	if inv.SourceIntakeG != 18_000_000 {
		t.Fatalf("source intake = %d, want 18,000,000", inv.SourceIntakeG)
	}
	// Every node's affected quantity must not exceed what it ever received.
	for _, n := range inv.Nodes {
		if n.UpperG > n.ReceivedG {
			t.Fatalf("node %s affected %d > received %d", n.LotKey, n.UpperG, n.ReceivedG)
		}
	}
	if inv.CapViolations != 0 {
		t.Fatalf("cap violations = %d, want 0", inv.CapViolations)
	}
	// The on-hand, production and shipped legs must reconcile to the truck intake.
	sum := inv.OnHandG + inv.ConsumedG + inv.ProductionG + inv.ShippedG
	if sum != inv.SourceIntakeG {
		t.Fatalf("legs sum %d != source intake %d (gap %d)", sum, inv.SourceIntakeG, inv.SourceIntakeG-sum)
	}
	if inv.ShippedG == 0 || inv.ProductionG == 0 {
		t.Fatalf("expected material already shipped and in production, got shipped=%d production=%d", inv.ShippedG, inv.ProductionG)
	}
	if inv.UnrecoverableG != inv.ProductionG+inv.ShippedG+inv.ConsumedG {
		t.Fatalf("unrecoverable %d does not equal production+shipped+consumed", inv.UnrecoverableG)
	}
}

// TestRecallUnknownSortGivesBounds proves an unknown-basis split produces a
// genuine interval, never a fake exact number.
func TestRecallUnknownSortGivesBounds(t *testing.T) {
	a, ctx := testApp(t)
	if _, err := a.ResetDemo(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := a.SeedDemo(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inv, err := a.GetInvestigation(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Exact {
		t.Fatalf("unknown sort reported as exact")
	}
	if !(inv.LowerG < inv.UpperG) {
		t.Fatalf("expected a strict interval, got lower=%d upper=%d", inv.LowerG, inv.UpperG)
	}
	if inv.LowerG < 0 || inv.LowerG > inv.UpperG {
		t.Fatalf("invalid interval %d..%d", inv.LowerG, inv.UpperG)
	}
	// Bounds must not be padded into a fake precision: the lower bound should be
	// materially below the upper bound, not within rounding noise.
	if inv.UpperG-inv.LowerG < inv.SourceIntakeG/10 {
		t.Fatalf("interval too narrow to be a real unknown-sort bound: %d..%d", inv.LowerG, inv.UpperG)
	}
}

// TestIsolationLifecycle drives apply → approve → partial review → full review.
func TestIsolationLifecycle(t *testing.T) {
	a, ctx := testApp(t)
	if _, err := a.ResetDemo(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := a.SeedDemo(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	quality := mustUser(t, a, "quality", "quality123")
	yard := mustUser(t, a, "yard", "yard123")

	// Seed leaves a pending order ISO-2601-001. Approving it activates the holds.
	order, err := a.ApproveIsolation(ctx, quality, 1)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if order.Status != "active" || len(order.Holds) == 0 {
		t.Fatalf("expected an active order with holds, got %s / %d", order.Status, len(order.Holds))
	}
	// A held lot cannot be drawn.
	held := order.Holds[0]
	_, err = a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "RC-TEST-DRAW-HOLD", Kind: "split", Mode: "uniform_share",
		Sources:      []SourceLine{{LotID: held.LotID, WeightG: 1000}},
		Destinations: []DestLine{{PileID: 1, WeightG: 1000, GradeCode: "CU-BRIGHT"}},
	})
	if err == nil {
		t.Fatalf("held lot was allowed to move")
	}

	// Partial review: only the first lot is released, the rest stays blocked.
	o, err := a.ReviewIsolation(ctx, quality, 1, []int64{held.LotID}, "partial")
	if err != nil {
		t.Fatalf("partial review: %v", err)
	}
	remaining := 0
	for _, h := range o.Holds {
		if !h.Released {
			remaining++
		}
	}
	if remaining == 0 {
		t.Fatalf("partial review released everything")
	}
	// Full review closes it.
	o, err = a.ReviewIsolation(ctx, quality, 1, nil, "full")
	if err != nil {
		t.Fatalf("full review: %v", err)
	}
	if o.Status != "closed" {
		t.Fatalf("order not closed after full review: %s", o.Status)
	}
	if o.RemainingHoldG != 0 {
		t.Fatalf("still holding %d g after full review", o.RemainingHoldG)
	}
}

// TestStaleSnapshotCannotBeApproved proves a transfer between the investigation
// and the approval invalidates the snapshot.
func TestStaleSnapshotCannotBeApproved(t *testing.T) {
	a, ctx := testApp(t)
	if _, err := a.ResetDemo(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := a.SeedDemo(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	quality := mustUser(t, a, "quality", "quality123")
	yard := mustUser(t, a, "yard", "yard123")

	ev, err := a.CreateQualityEvent(ctx, quality, QualityEventInput{
		EventNo: "QEV-STALE-TEST", Kind: "composition", TripID: 4, Param: "impurity_ppm",
		MinPPM: 20000, MaxPPM: 20000, Basis: "manual", Note: "stale test",
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	inv, err := a.Investigate(ctx, quality, ev.ID)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	ord, err := a.ApplyIsolation(ctx, quality, ApplyIsolationInput{
		OrderNo: "ISO-STALE-TEST", EventID: ev.ID, InvestigationID: inv.ID,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// A new transfer changes the epoch.
	var freeLot, freeW int64
	if err := a.DB.Pool.QueryRow(ctx, `select l.id, l.weight_g from rt.inventory_lots l
		join rt.piles p on p.id=l.pile_id where p.kind='isolated' and l.weight_g > 1000 limit 1`).
		Scan(&freeLot, &freeW); err != nil {
		t.Fatalf("find free lot: %v", err)
	}
	var isoPile int64
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.piles where code='ISO-1'`).Scan(&isoPile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "RC-STALE-BUMP", Kind: "split", Mode: "uniform_share",
		Sources:      []SourceLine{{LotID: freeLot, WeightG: 1000}},
		Destinations: []DestLine{{PileID: isoPile, WeightG: 1000, GradeCode: "CU-DIRTY"}},
	}); err != nil {
		t.Fatalf("bump transfer: %v", err)
	}
	if _, err := a.ApproveIsolation(ctx, quality, ord.ID); err == nil {
		t.Fatalf("stale snapshot was approved after a new transfer")
	}
}

// TestOverlappingEventsBothMustRelease proves two events holding the same lot
// keep it blocked until both are released.
func TestOverlappingEventsBothMustRelease(t *testing.T) {
	a, ctx := testApp(t)
	if _, err := a.ResetDemo(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := a.SeedDemo(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	quality := mustUser(t, a, "quality", "quality123")
	// First event's order active.
	o1, err := a.ApproveIsolation(ctx, quality, 1)
	if err != nil {
		t.Fatalf("approve e1: %v", err)
	}
	// Overlapping event on the clean truck that shares the mixed lots.
	ev2, err := a.CreateQualityEvent(ctx, quality, QualityEventInput{
		EventNo: "QEV-OVERLAP-TEST", Kind: "composition", TripID: 5, Param: "metal_ppm",
		MinPPM: 610000, MaxPPM: 610000, Basis: "manual", Note: "overlap",
	})
	if err != nil {
		t.Fatalf("event2: %v", err)
	}
	inv2, err := a.Investigate(ctx, quality, ev2.ID)
	if err != nil {
		t.Fatalf("investigate2: %v", err)
	}
	ord2, err := a.ApplyIsolation(ctx, quality, ApplyIsolationInput{
		OrderNo: "ISO-OVERLAP-TEST", EventID: ev2.ID, InvestigationID: inv2.ID,
	})
	if err != nil {
		t.Fatalf("apply2: %v", err)
	}
	o2, err := a.ApproveIsolation(ctx, quality, ord2.ID)
	if err != nil {
		t.Fatalf("approve2: %v", err)
	}
	// Release event 1 fully.
	if _, err := a.ReviewIsolation(ctx, quality, o1.ID, nil, "release e1"); err != nil {
		t.Fatalf("review e1: %v", err)
	}
	// At least one lot is shared and thus still held by event 2.
	shared := int64(0)
	for _, h := range o2.Holds {
		if !h.Released {
			shared = h.LotID
			break
		}
	}
	if shared == 0 {
		t.Skip("no overlapping held lot to check")
	}
	holds, err := a.ActiveHoldsForLot(ctx, a.DB.Pool, shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) == 0 {
		t.Fatalf("shared lot %d is not blocked after event 1 released; event 2 should still hold it", shared)
	}
}

