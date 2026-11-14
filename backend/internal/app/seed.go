package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"recycletrace/backend/internal/num"
)

// ResetDemo clears all business data and reseeds master data/users. It is used
// only by the seed command; the server never calls it.
func (a *App) ResetDemo(ctx context.Context) (string, error) {
	_, err := a.DB.Pool.Exec(ctx, `truncate rt.audit_log, rt.payment_ledger, rt.payables, rt.transfer_lines,
		rt.lot_events, rt.lot_links, rt.lot_origins, rt.loss_attribution, rt.inventory_lots, rt.movements, rt.grade_approvals,
		rt.trip_assay_adoptions, rt.assay_results, rt.samples, rt.weight_adoptions, rt.weighings, rt.trips,
		rt.contract_grades, rt.contracts, rt.piles, rt.suppliers restart identity cascade`)
	if err != nil {
		return "", err
	}
	_, err = a.DB.Pool.Exec(ctx, `truncate rt.sessions, rt.users restart identity cascade`)
	if err != nil {
		return "", err
	}
	return "truncated", nil
}

// SeedDemo writes one coherent story:
//
//	One supplier sends two trucks of copper scrap on the same day (B-...-001 and
//	B-...-002). Each truck has its own sealed sample bound to a trip and a
//	sampling position. Truck 2 is re-weighed (second gross) and the yard adopts
//	the second gross with a written basis; the original net is kept.
//	Truck 2's assay is re-tested downward by quality after intake; the payable
//	is re-computed and a new version shows the old/new weight, metal and amount.
//	Both trucks are mixed into one usable pile, then part of the mixed pile is
//	split off to feed the furnace, with a declared drying loss.
//	A third truck is isolated (high impurity): it cannot enter the usable pile,
//	and mixing it with usable material is rejected.
//	Two yard operators race to take the same remaining balance; exactly one wins.
func (a *App) SeedDemo(ctx context.Context) error {
	users, err := a.seedUsers(ctx)
	if err != nil {
		return err
	}
	purchasing, yard, lab, quality, finance := users["purchasing"], users["yard"], users["lab"], users["quality"], users["finance"]
	if err := a.seedMaster(ctx); err != nil {
		return err
	}

	day := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	// ---------------- supplier A, contract RT-STD-DRY ----------------
	supA, contractA, piles, err := a.demoRefs(ctx)
	if err != nil {
		return err
	}
	trip1, err := a.CreateTrip(ctx, purchasing, CreateTripInput{
		SupplierID: supA, ContractID: contractA, PlateNo: "沪A-89321", Driver: "赵师傅",
		ArrivalAt: day.Add(0 * time.Minute), Note: "第一车 废铜(紫铜含量较高)"})
	if err != nil {
		return fmt.Errorf("trip1: %w", err)
	}
	trip2, err := a.CreateTrip(ctx, purchasing, CreateTripInput{
		SupplierID: supA, ContractID: contractA, PlateNo: "沪A-77106", Driver: "钱师傅",
		ArrivalAt: day.Add(40 * time.Minute), Note: "第二车 同日同供应商，独立批次"})
	if err != nil {
		return fmt.Errorf("trip2: %w", err)
	}
	isolatedTrip, err := a.CreateTrip(ctx, purchasing, CreateTripInput{
		SupplierID: supA, ContractID: contractA, PlateNo: "苏E-30217", Driver: "孙师傅",
		ArrivalAt: day.Add(95 * time.Minute), Note: "第三车 杂质偏高，需隔离"})
	if err != nil {
		return fmt.Errorf("trip3: %w", err)
	}

	// ---------------- weighings ----------------
	g1, err := a.AddWeighing(ctx, yard, trip1.ID, Weighing{Kind: "gross", WeightG: 41_820_000,
		TicketNo: "T2601-0001", WeighedAt: day.Add(2 * time.Minute), ScaleDevice: "SCALE-N-01"})
	if err != nil {
		return err
	}
	t1, err := a.AddWeighing(ctx, yard, trip1.ID, Weighing{Kind: "tare", WeightG: 13_240_000,
		TicketNo: "T2601-0002", WeighedAt: day.Add(9 * time.Minute), ScaleDevice: "SCALE-N-01"})
	if err != nil {
		return err
	}
	if _, err := a.AdoptWeights(ctx, yard, trip1.ID, g1.ID, t1.ID, "首磅毛重 41,820kg，皮重 13,240kg，净重 28,580kg（31,582 采用）"); err != nil {
		return err
	}

	g2a, err := a.AddWeighing(ctx, yard, trip2.ID, Weighing{Kind: "gross", WeightG: 33_450_000,
		TicketNo: "T2601-0003", WeighedAt: day.Add(42 * time.Minute), ScaleDevice: "SCALE-N-02"})
	if err != nil {
		return err
	}
	// Second gross weighing on the same truck: the scale was re-zeroed.
	g2b, err := a.AddWeighing(ctx, yard, trip2.ID, Weighing{Kind: "gross", WeightG: 33_380_000,
		TicketNo: "T2601-0004", WeighedAt: day.Add(52 * time.Minute), ScaleDevice: "SCALE-N-02"})
	if err != nil {
		return err
	}
	t2, err := a.AddWeighing(ctx, yard, trip2.ID, Weighing{Kind: "tare", WeightG: 12_820_000,
		TicketNo: "T2601-0005", WeighedAt: day.Add(58 * time.Minute), ScaleDevice: "SCALE-N-02"})
	if err != nil {
		return err
	}
	if _, err := a.AdoptWeights(ctx, yard, trip2.ID, g2a.ID, t2.ID,
		"首次采用首磅毛重 33,450kg，净重 20,630kg"); err != nil {
		return err
	}
	// Re-weigh adopted: original value survives, basis recorded.
	if _, err := a.AdoptWeights(ctx, yard, trip2.ID, g2b.ID, t2.ID,
		"复称：地磅零点复核后以第二次毛重 33,380kg 为准（原毛重 33,450kg 保留原值，依据：SCALE-N-02 零点校正单 Q-1108）"); err != nil {
		return err
	}

	g3, err := a.AddWeighing(ctx, yard, isolatedTrip.ID, Weighing{Kind: "gross", WeightG: 24_610_000,
		TicketNo: "T2601-0006", WeighedAt: day.Add(97 * time.Minute), ScaleDevice: "SCALE-N-01"})
	if err != nil {
		return err
	}
	t3, err := a.AddWeighing(ctx, yard, isolatedTrip.ID, Weighing{Kind: "tare", WeightG: 13_000_000,
		TicketNo: "T2601-0007", WeighedAt: day.Add(104 * time.Minute), ScaleDevice: "SCALE-N-01"})
	if err != nil {
		return err
	}
	if _, err := a.AdoptWeights(ctx, yard, isolatedTrip.ID, g3.ID, t3.ID, "净重 11,610kg"); err != nil {
		return err
	}

	// ---------------- sealed samples (trip + position bound) ----------------
	s1, err := a.CreateSample(ctx, lab, trip1.ID, CreateSampleInput{SampleNo: "SMP-2601-001", Position: "车厢前部", SealNo: "SEAL-8811",
		SealedAt: day.Add(15 * time.Minute), Note: "三点取样，本封对应车厢前部"})
	if err != nil {
		return err
	}
	s2, err := a.CreateSample(ctx, lab, trip2.ID, CreateSampleInput{SampleNo: "SMP-2601-002", Position: "车厢中部", SealNo: "SEAL-8812",
		SealedAt: day.Add(60 * time.Minute), Note: "同供应商同日，但封样绑定第二车的取样位置"})
	if err != nil {
		return err
	}
	s3, err := a.CreateSample(ctx, lab, isolatedTrip.ID, CreateSampleInput{SampleNo: "SMP-2601-003", Position: "车厢后部", SealNo: "SEAL-8813",
		SealedAt: day.Add(106 * time.Minute), Note: "外观含明显杂物，预判隔离"})
	if err != nil {
		return err
	}

	// ---------------- assays ----------------
	// Truck 1: 3.2% moisture, 1.1% impurity (dry), 62.5% metal (dry).
	a1, err := a.AddAssay(ctx, lab, s1.ID, Assay{MoisturePPM: 32_000, ImpurityPPM: i64p(11_000),
		ImpurityBasis: "dry", MetalPPM: 625_000, MetalBasis: "dry", Inspector: "李化验"})
	if err != nil {
		return err
	}
	// Truck 2: 3.0% moisture, 1.4% impurity (dry), 61.8% metal (dry).
	a2, err := a.AddAssay(ctx, lab, s2.ID, Assay{MoisturePPM: 30_000, ImpurityPPM: i64p(14_000),
		ImpurityBasis: "dry", MetalPPM: 618_000, MetalBasis: "dry", Inspector: "李化验"})
	if err != nil {
		return err
	}
	// Truck 3: 5.6% moisture, 6.2% impurity, 52% metal — high impurity.
	a3, err := a.AddAssay(ctx, lab, s3.ID, Assay{MoisturePPM: 56_000, ImpurityPPM: i64p(62_000),
		ImpurityBasis: "dry", MetalPPM: 520_000, MetalBasis: "dry", Inspector: "李化验"})
	if err != nil {
		return err
	}

	if _, err := a.AdoptAssay(ctx, quality, a1.ID, "复看封样照片与取样位置一致，采用"); err != nil {
		return err
	}
	if _, err := a.AdoptAssay(ctx, quality, a2.ID, "采用，注意第二车曾复称，品位照常用"); err != nil {
		return err
	}
	if _, err := a.AdoptAssay(ctx, quality, a3.ID, "采用但含杂高，转隔离"); err != nil {
		return err
	}

	// ---------------- grade approvals ----------------
	if _, err := a.ApproveGrade(ctx, quality, trip1.ID, "CU-BRIGHT", false, "光亮铜，可入可用堆"); err != nil {
		return err
	}
	if _, err := a.ApproveGrade(ctx, quality, trip2.ID, "CU-BRIGHT", false, "光亮铜，可入可用堆"); err != nil {
		return err
	}
	if _, err := a.ApproveGrade(ctx, quality, isolatedTrip.ID, "CU-DIRTY", true, "含杂高，隔离堆，不得转可用"); err != nil {
		return err
	}

	// ---------------- intake ----------------
	if _, err := a.Intake(ctx, yard, IntakeInput{TripID: trip1.ID, PileID: piles["RECV-A"]}); err != nil {
		return err
	}
	if _, err := a.Intake(ctx, yard, IntakeInput{TripID: trip2.ID, PileID: piles["RECV-A"]}); err != nil {
		return err
	}
	if _, err := a.Intake(ctx, yard, IntakeInput{TripID: isolatedTrip.ID, PileID: piles["ISO-1"]}); err != nil {
		return err
	}

	// ---------------- payables v1 for both trucks ----------------
	for _, tr := range []Trip{trip1, trip2} {
		pv, err := a.ComputePayable(ctx, finance, tr.ID, "首次按采用称次与采用化验计算")
		if err != nil {
			return fmt.Errorf("payable v1 trip %d: %w", tr.ID, err)
		}
		if _, err := a.ApprovePayable(ctx, finance, pv.ID, "核对净重、品位与合同步骤后批准"); err != nil {
			return fmt.Errorf("approve v1 trip %d: %w", tr.ID, err)
		}
	}

	// ---------------- mix the two truck lots into a usable pile ----------------
	trip1LotID, err := a.lotIDForKey(ctx, fmt.Sprintf("TRIP-%d", trip1.ID))
	if err != nil {
		return err
	}
	trip2LotID, err := a.lotIDForKey(ctx, fmt.Sprintf("TRIP-%d", trip2.ID))
	if err != nil {
		return err
	}
	var w1, w2 int64
	if err := a.DB.Pool.QueryRow(ctx, `select weight_g from rt.inventory_lots where id=$1`, trip1LotID).Scan(&w1); err != nil {
		return err
	}
	if err := a.DB.Pool.QueryRow(ctx, `select weight_g from rt.inventory_lots where id=$1`, trip2LotID).Scan(&w2); err != nil {
		return err
	}
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "MX-2601-001", Kind: "mix", OccurredAt: day.Add(4 * time.Hour).Format(time.RFC3339),
		Mode: "uniform_share", Note: "两车料合堆至可用堆 #1，声明均匀混合，按各车占比分配",
		Sources: []SourceLine{
			{LotID: trip1LotID, WeightG: w1},
			{LotID: trip2LotID, WeightG: w2},
		},
		Destinations: []DestLine{{PileID: piles["USABLE-1"], WeightG: w1 + w2, GradeCode: "CU-BRIGHT"}},
	}); err != nil {
		return fmt.Errorf("mix: %w", err)
	}

	// ---------------- split part of the mixed pile to the furnace (drying loss) ----------------
	var mixedLotID int64
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.inventory_lots where lot_key like 'mix-%'`).Scan(&mixedLotID); err != nil {
		return err
	}
	var mixedW, mixedM int64
	if err := a.DB.Pool.QueryRow(ctx, `select weight_g, metal_mg from rt.inventory_lots where id=$1`, mixedLotID).Scan(&mixedW, &mixedM); err != nil {
		return err
	}
	splitW := mixedW / 2
	dryLoss := num.ApplyPPM(splitW, 6_000) // 0.6% moisture loss on the split portion, water only
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "SP-2601-001", Kind: "split", OccurredAt: day.Add(28 * time.Hour).Format(time.RFC3339),
		Mode: "uniform_share", Note: "拆出一半投炉；声明干燥损失 0.6%，仅计重量损耗，不减金属",
		Sources:      []SourceLine{{LotID: mixedLotID, WeightG: splitW}},
		Destinations: []DestLine{{PileID: piles["FURNACE"], WeightG: splitW - dryLoss, GradeCode: "CU-BRIGHT"}},
		Losses:       []LossLine{{WeightG: dryLoss, LossClass: "drying", Note: "库内风干析出水分"}},
	}); err != nil {
		return fmt.Errorf("split: %w", err)
	}

	// ---------------- isolated material cannot be mixed into usable ----------------
	var isoLotID int64
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.inventory_lots where lot_key like 'TRIP-%' and trip_id=$1`,
		isolatedTrip.ID).Scan(&isoLotID); err != nil {
		return err
	}
	var isoW int64
	if err := a.DB.Pool.QueryRow(ctx, `select weight_g from rt.inventory_lots where id=$1`, isoLotID).Scan(&isoW); err != nil {
		return err
	}
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "MX-2601-ISO-TRY", Kind: "mix", Mode: "uniform_share",
		Note:         "隔离料尝试混入可用堆（应被拒绝）",
		Sources:      []SourceLine{{LotID: isoLotID, WeightG: isoW}},
		Destinations: []DestLine{{PileID: piles["USABLE-1"], WeightG: isoW, GradeCode: "CU-DIRTY"}},
	}); err == nil {
		return fmt.Errorf("isolated material was allowed into a usable pile")
	}

	// ---------------- quality re-test lowers truck 1 ----------------
	// Lab submits a new version on the same sealed sample (position unchanged).
	re, err := a.AddAssay(ctx, lab, s1.ID, Assay{MoisturePPM: 32_000, ImpurityPPM: i64p(16_500),
		ImpurityBasis: "dry", MetalPPM: 612_000, MetalBasis: "dry", Inspector: "李化验"})
	if err != nil {
		return err
	}
	if _, err := a.AdoptAssay(ctx, quality, re.ID, "复检：金属比例下调至 61.2%，杂物上调至 1.65%，采用"); err != nil {
		return err
	}
	// Re-assay propagates to usable + furnace lots in place (production is not reverted).
	rr, err := a.ApplyReassay(ctx, quality, trip1.ID)
	if err != nil {
		return fmt.Errorf("reassay: %w", err)
	}
	if rr.Payable == nil {
		return fmt.Errorf("reassay produced no payable preview")
	}
	if rr.Payable.DeltaAmountCents >= 0 {
		return fmt.Errorf("expected a downward payable delta, got %d", rr.Payable.DeltaAmountCents)
	}

	// ---------------- new payable version after re-test ----------------
	pv2, err := a.ComputePayable(ctx, finance, trip1.ID, "复检下调后按新采用化验重算（原单保留）")
	if err != nil {
		return fmt.Errorf("payable v2: %w", err)
	}
	if _, err := a.ApprovePayable(ctx, finance, pv2.ID, "已核对新旧净重/金属量/应付差额，批准调整"); err != nil {
		return err
	}

	// ---------------- race on the same remaining balance ----------------
	// Two dispatch requests both name the whole remaining furnace balance. Only
	// the first can succeed; the second must be blocked for over-issuing stock.
	var remainW int64
	if err := a.DB.Pool.QueryRow(ctx, `select weight_g from rt.inventory_lots where id=$1`, mixedLotID).Scan(&remainW); err != nil {
		return err
	}
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "RL-2601-001-A", Kind: "ship", Mode: "uniform_share",
		Note:         "操作员甲抢到可用堆剩余量，整批出库至发货区",
		Sources:      []SourceLine{{LotID: mixedLotID, WeightG: remainW}},
		Destinations: []DestLine{{PileID: piles["SHIP-1"], WeightG: remainW, GradeCode: "CU-BRIGHT"}},
	}); err != nil {
		return fmt.Errorf("dispatch A: %w", err)
	}
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "RL-2601-001-B", Kind: "ship", Mode: "uniform_share",
		Note:         "操作员乙同时申请同一余量（应因余额为零被阻断）",
		Sources:      []SourceLine{{LotID: mixedLotID, WeightG: remainW}},
		Destinations: []DestLine{{PileID: piles["SHIP-1"], WeightG: remainW, GradeCode: "CU-BRIGHT"}},
	}); err == nil {
		return fmt.Errorf("second dispatch on the same balance was allowed")
	}

	// ---------------- replaying the same transfer does not move twice ----------------
	before := a.pileWeight(ctx, piles["SHIP-1"])
	if _, err := a.Transfer(ctx, yard, TransferInput{
		ExternalRef: "MX-2601-001", Kind: "mix", Mode: "uniform_share", Note: "重放同一转移单",
		Sources:      []SourceLine{{LotID: trip1LotID, WeightG: 1}},
		Destinations: []DestLine{{PileID: piles["USABLE-1"], WeightG: 1, GradeCode: "CU-BRIGHT"}},
	}); err != nil {
		return fmt.Errorf("replay rejected instead of returning original: %w", err)
	}
	if after := a.pileWeight(ctx, piles["SHIP-1"]); after != before {
		return fmt.Errorf("replay moved material: %d -> %d", before, after)
	}
	return nil
}

func (a *App) lotIDForKey(ctx context.Context, key string) (int64, error) {
	var id int64
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.inventory_lots where lot_key=$1`, key).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, notFound("lot key %q", key)
		}
		return 0, err
	}
	return id, nil
}

func (a *App) pileWeight(ctx context.Context, pileID int64) int64 {
	var w int64
	_ = a.DB.Pool.QueryRow(ctx, `select coalesce(sum(weight_g),0) from rt.inventory_lots where pile_id=$1`, pileID).Scan(&w)
	return w
}

func i64p(v int64) *int64 { return &v }

func (a *App) seedUsers(ctx context.Context) (map[string]User, error) {
	defs := []struct {
		username, display, role, password string
	}{
		{"buyer", "采购员小王", "purchasing", "buyer123"},
		{"yard", "料场长老张", "yard", "yard123"},
		{"lab", "化验员小李", "lab", "lab123"},
		{"quality", "质量主管老陈", "quality", "quality123"},
		{"finance", "财务小周", "finance", "finance123"},
		{"admin", "系统管理员", "admin", "admin123"},
	}
	out := map[string]User{}
	for _, d := range defs {
		hash, err := HashPassword(d.password)
		if err != nil {
			return nil, err
		}
		var u User
		err = a.DB.Pool.QueryRow(ctx, `insert into rt.users(username, display_name, role, password_hash)
			values($1,$2,$3,$4) returning id, username, display_name, role`,
			d.username, d.display, d.role, hash).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role)
		if err != nil {
			return nil, err
		}
		out[d.username] = u
		out[d.role] = u
	}
	return out, nil
}

func (a *App) seedMaster(ctx context.Context) error {
	_, err := a.DB.Pool.Exec(ctx, `
		insert into rt.suppliers(code, name) values ('SUP-A','华东再生金属有限公司'), ('SUP-B','江南铜业原料部');
		insert into rt.piles(code, name, kind, physical_location) values
			('RECV-A','来料接收区 A','receiving','料场北侧'),
			('ISO-1','隔离料堆','isolated','料场西南角'),
			('USABLE-1','可用铜料堆 #1','usable','料场中区'),
			('USABLE-2','可用铜料堆 #2','usable','料场东区'),
			('FURNACE','投炉区','production','熔炼车间'),
			('SHIP-1','出库发货区','shipped','成品仓'),
			('LOSS-1','损耗计量区','loss','计量台账');
		insert into rt.contracts(code, supplier_id, name, metal_basis, unit_price_cents_per_t, formula, note)
			select 'RT-STD-DRY', id, '再生铜标准合同（干基金属）', 'dry', 5800000,
				'{"code":"DRY-IMP-METAL"}', '湿净重先扣水得干重，干重扣已单列非金属杂物，剩余乘干基金属比例'
			from rt.suppliers where code='SUP-A';
		insert into rt.contracts(code, supplier_id, name, metal_basis, unit_price_cents_per_t, formula, note)
			select 'RT-WET-NET', id, '再生铜合同（湿基计价，独立公式）', 'wet', 5500000,
				'{"code":"WET-NET-METAL"}', '另一基准，必须使用独立公式，不套用干基比例'
			from rt.suppliers where code='SUP-B';
		insert into rt.contract_grades(contract_id, grade_code, coef_ppm, intake_allowed)
			select id, 'CU-BRIGHT', 1000000, true from rt.contracts where code='RT-STD-DRY';
		insert into rt.contract_grades(contract_id, grade_code, coef_ppm, intake_allowed)
			select id, 'CU-MIX', 960000, true from rt.contracts where code='RT-STD-DRY';
		insert into rt.contract_grades(contract_id, grade_code, coef_ppm, intake_allowed)
			select id, 'CU-DIRTY', 820000, false from rt.contracts where code='RT-STD-DRY';
		insert into rt.contract_grades(contract_id, grade_code, coef_ppm, intake_allowed)
			select id, 'CU-BRIGHT', 1000000, true from rt.contracts where code='RT-WET-NET';
	`)
	return err
}

func (a *App) demoRefs(ctx context.Context) (int64, int64, map[string]int64, error) {
	var supA, contractA int64
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.suppliers where code='SUP-A'`).Scan(&supA); err != nil {
		return 0, 0, nil, err
	}
	if err := a.DB.Pool.QueryRow(ctx, `select id from rt.contracts where code='RT-STD-DRY'`).Scan(&contractA); err != nil {
		return 0, 0, nil, err
	}
	piles := map[string]int64{}
	rows, err := a.DB.Pool.Query(ctx, `select code, id from rt.piles`)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			return 0, 0, nil, err
		}
		piles[code] = id
	}
	return supA, contractA, piles, rows.Err()
}
