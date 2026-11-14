package app

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"recycletrace/backend/internal/num"
)

// ---------------------------------------------------------------------------
// 质量事件 + 下游排查
//
// 排查不做“沿来源递归累加”。它沿每个带数量的转移边按实际流量传播：
//   * 每个节点记录历史接收量 received，影响量永远不超过 received；
//   * 汇合节点按来源流量归并（多个上游边分别携带各自的一份），不会把同一来源算两遍；
//   * 均匀混合/来源实测边按占比拆分；basis=unknown 的边不确定拆分，给出下界/上界；
//   * 存量、已耗用、在制、已发分别汇总，并与源车历史接收量核对。
// ---------------------------------------------------------------------------

type QualityEvent struct {
	ID            int64   `json:"id"`
	EventNo       string  `json:"event_no"`
	Kind          string  `json:"kind"`
	TripID        int64   `json:"trip_id"`
	BatchNo       string  `json:"batch_no"`
	Param         string  `json:"param"`
	MinPPM        int64   `json:"min_ppm"`
	MaxPPM        int64   `json:"max_ppm"`
	Basis         string  `json:"basis"`
	Note          string  `json:"note"`
	ReportedBy    int64   `json:"reported_by"`
	ReportedAt    string  `json:"reported_at"`
	Status        string  `json:"status"`
	LatestInvestigationID *int64 `json:"latest_investigation_id,omitempty"`
	LatestSeq     int     `json:"latest_seq"`
}

type QualityEventInput struct {
	EventNo string `json:"event_no"`
	Kind    string `json:"kind"`
	TripID  int64  `json:"trip_id"`
	// TripKey lets a sample file name the truck by its stable batch number; when
	// trip_id is absent it is resolved to the trip id. One of the two is required.
	TripKey string `json:"trip_key"`
	Param   string `json:"param"`
	MinPPM  int64  `json:"min_ppm"`
	MaxPPM  int64  `json:"max_ppm"`
	Basis   string `json:"basis"`
	Note    string `json:"note"`
}

// RecallPathStep is one step of a route from the source truck to an affected node.
type RecallPathStep struct {
	MovementID  int64  `json:"movement_id"`
	MovementRef string `json:"movement_ref"`
	FromLotKey  string `json:"from_lot_key"`
	ToLotKey    string `json:"to_lot_key"`
	EdgeWeightG int64  `json:"edge_weight_g"`
	Basis       string `json:"basis"`
	LowerG      int64  `json:"lower_g"`
	UpperG      int64  `json:"upper_g"`
}

// RecallNode is one stock node holding (or having held) the source truck's material.
type RecallNode struct {
	LotID     int64            `json:"lot_id"`
	LotKey    string           `json:"lot_key"`
	PileName  string           `json:"pile_name"`
	PileKind  string           `json:"pile_kind"`
	State     string           `json:"state"`
	Bucket    string           `json:"bucket"`
	ReceivedG int64            `json:"received_g"`
	WeightG   int64            `json:"weight_g"`
	LowerG    int64            `json:"lower_g"`
	UpperG    int64            `json:"upper_g"`
	Basis     string           `json:"basis"`
	Route     []RecallPathStep `json:"route"`
}

// RecallAnalysis is the deterministic result of one propagation pass.
type RecallAnalysis struct {
	Param    string
	LowerPPM int64
	UpperPPM int64
	Exact    bool

	OnHandG            int64
	OnHandLowerG       int64
	OnHandComponentMg  int64
	ConsumedG          int64
	ConsumedLowerG     int64
	ConsumedComponentMg int64
	ProductionG        int64
	ProductionLowerG   int64
	ProductionComponentMg int64
	ShippedG           int64
	ShippedLowerG      int64
	ShippedComponentMg int64
	LossG              int64
	LossLowerG         int64
	LossComponentMg    int64

	UnrecoverableG int64
	SourceIntakeG  int64
	CheckedG       int64
	LedgerG        int64
	GapG           int64
	LowerG         int64
	UpperG         int64
	LowerComponentMg int64
	UpperComponentMg int64
	CapViolations  int
	Nodes          []RecallNode
}

type recallLot struct {
	ID       int64
	Key      string
	PileName string
	PileKind string
	WeightG  int64
	State    string
	TripID   int64
}

type recallEdge struct {
	MovementID int64
	Ref        string
	From       int64
	To         int64
	FromKey    string
	ToKey      string
	WeightG    int64
	Basis      string
	LowerG     int64
	UpperG     int64
}

type interval struct{ lo, hi int64 }

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// analyzeRecall performs the flow propagation for one event.
func analyzeRecall(ctx context.Context, q querier, tripID int64, param string, ppmLo, ppmHi int64) (RecallAnalysis, error) {
	out := RecallAnalysis{Param: param, LowerPPM: ppmLo, UpperPPM: ppmHi, Nodes: []RecallNode{}}
	if ppmHi < ppmLo {
		ppmHi = ppmLo
	}

	lots := map[int64]*recallLot{}
	rows, err := q.Query(ctx, `select l.id, l.lot_key, p.name, p.kind, l.weight_g, l.state, coalesce(l.trip_id,0)
		from rt.inventory_lots l join rt.piles p on p.id=l.pile_id order by l.id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var l recallLot
		if err := rows.Scan(&l.ID, &l.Key, &l.PileName, &l.PileKind, &l.WeightG, &l.State, &l.TripID); err != nil {
			rows.Close()
			return out, err
		}
		lots[l.ID] = &l
	}
	rows.Close()

	edges := []recallEdge{}
	rows, err = q.Query(ctx, `select lk.movement_id, coalesce(m.external_ref,''), lk.from_lot_id, lk.to_lot_id,
		f.lot_key, t.lot_key, lk.weight_g, lk.basis
		from rt.lot_links lk
		join rt.inventory_lots f on f.id=lk.from_lot_id
		join rt.inventory_lots t on t.id=lk.to_lot_id
		left join rt.movements m on m.id=lk.movement_id order by lk.id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e recallEdge
		if err := rows.Scan(&e.MovementID, &e.Ref, &e.From, &e.To, &e.FromKey, &e.ToKey, &e.WeightG, &e.Basis); err != nil {
			rows.Close()
			return out, err
		}
		edges = append(edges, e)
	}
	rows.Close()

	// Adjacency and incoming sums.
	outEdges := map[int64][]int{}
	inWeight := map[int64]int64{}
	inCount := map[int64]int{}
	for i, e := range edges {
		outEdges[e.From] = append(outEdges[e.From], i)
		inWeight[e.To] += e.WeightG
		inCount[e.To]++
	}

	// Seeds: this trip's own intake lots (lots with the trip id and no incoming edge).
	seeds := []int64{}
	var sourceG int64
	for id, l := range lots {
		if l.TripID != tripID || inCount[id] > 0 {
			continue
		}
		var outTotal int64
		for _, ei := range outEdges[id] {
			outTotal += edges[ei].WeightG
		}
		received := l.WeightG + outTotal
		if received <= 0 {
			continue
		}
		seeds = append(seeds, id)
		sourceG += received
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i] < seeds[j] })
	out.SourceIntakeG = sourceG
	if len(seeds) == 0 {
		out.Exact = true
		return out, nil
	}

	received := map[int64]int64{}
	infl := map[int64]*interval{}
	prevEdge := map[int64]int{}
	hasPrev := map[int64]bool{}

	// Reachable sub-graph: everything downstream of the seeds.
	nodesSet := map[int64]bool{}
	queue := []int64{}
	for _, id := range seeds {
		nodesSet[id] = true
		queue = append(queue, id)
	}
	for i := 0; i < len(queue); i++ {
		from := queue[i]
		for _, ei := range outEdges[from] {
			to := edges[ei].To
			if !nodesSet[to] {
				nodesSet[to] = true
				queue = append(queue, to)
			}
		}
	}
	for id := range nodesSet {
		received[id] = inWeight[id]
	}
	// Seed nodes are historical receivers too: what it still holds plus what it
	// has ever sent downstream. Using the full ledger prevents an origin from
	// exceeding the volume that actually arrived.
	for _, id := range seeds {
		var outTotal int64
		for _, ei := range outEdges[id] {
			outTotal += edges[ei].WeightG
		}
		r := lots[id].WeightG + outTotal
		received[id] = r
		infl[id] = &interval{lo: r, hi: r}
	}

	// Kahn topological order restricted to reachable edges.
	reachIndeg := map[int64]int{}
	for _, e := range edges {
		if nodesSet[e.From] && nodesSet[e.To] {
			reachIndeg[e.To]++
		}
	}
	order := []int64{}
	q2 := append([]int64(nil), seeds...)
	for i := 0; i < len(q2); i++ {
		n := q2[i]
		order = append(order, n)
		for _, ei := range outEdges[n] {
			to := edges[ei].To
			if !nodesSet[to] {
				continue
			}
			reachIndeg[to]--
			if reachIndeg[to] == 0 {
				q2 = append(q2, to)
			}
		}
	}

	// Propagate.
	for _, id := range order {
		lot := lots[id]
		r := received[id]
		iv := infl[id]
		if iv == nil {
			continue
		}
		if r <= 0 {
			continue
		}
		lo := clamp(iv.lo, 0, r)
		hi := clamp(iv.hi, 0, r)
		if lo > hi {
			lo = hi
		}
		// Of the historical receipt r, what is still here (current balance) and
		// what has been pushed downstream (all outgoing edges, ever). This keeps
		// the two buckets from double counting and works for fully consumed nodes.
		histOut := make([]int64, len(outEdges[id]))
		var histOutSum int64
		for k, ei := range outEdges[id] {
			histOut[k] = edges[ei].WeightG
			histOutSum += histOut[k]
		}
		remainW := r - histOutSum
		if remainW < 0 {
			remainW = 0
		}
		if remainW > lot.WeightG {
			remainW = lot.WeightG
		}
		remLo := num.MulDiv(lo, remainW, r)
		remHi := num.MulDiv(hi, remainW, r)
		outLo := lo - remLo
		outHi := hi - remHi
		iv.lo, iv.hi = remLo, remHi

		es := outEdges[id]
		if len(es) == 0 || outHi <= 0 {
			continue
		}
		if outLo > outHi {
			outLo = outHi
		}
		// Declared edges (uniform/measured) carry this node's composition exactly.
		// Unknown-basis edges absorb the remainder, with genuine uncertainty. Splitting
		// the two classes avoids letting an unknown edge's wide cap distort a known
		// share, while still bounding the unknown branches.
		knownIdx, unknownIdx := []int{}, []int{}
		for k, ei := range es {
			if edges[ei].Basis == "unknown" {
				unknownIdx = append(unknownIdx, k)
			} else {
				knownIdx = append(knownIdx, k)
			}
		}
		hAll := make([]int64, len(es))
		lAll := make([]int64, len(es))
		var knownHi, knownLo int64
		for _, k := range knownIdx {
			h := num.MulDiv(outHi, histOut[k], histOutSum)
			l := num.MulDiv(outLo, histOut[k], histOutSum)
			if h > edges[es[k]].WeightG {
				h = edges[es[k]].WeightG
			}
			if l > h {
				l = h
			}
			hAll[k], lAll[k] = h, l
			knownHi += h
			knownLo += l
		}
		unkHi := outHi - knownHi
		unkLo := outLo - knownLo
		if unkHi < 0 {
			unkHi = 0
		}
		if unkLo < 0 {
			unkLo = 0
		}
		if len(unknownIdx) > 0 && unkHi > 0 {
			// A source unknown sort admits any split: each unknown edge can carry
			// anywhere from "everything the others leave to it" to "all of the
			// material that is still un-sorted". We do NOT normalise these caps,
			// because that would invent precision the sort does not have.
			for j, k := range unknownIdx {
				cap := edges[es[k]].WeightG
				if cap > unkHi {
					cap = unkHi
				}
				others := int64(0)
				for o, ok2 := range unknownIdx {
					if o == j {
						continue
					}
					oc := edges[es[ok2]].WeightG
					if oc > unkHi {
						oc = unkHi
					}
					others += oc
				}
				lo := unkLo - others
				if lo < 0 {
					lo = 0
				}
				if lo > cap {
					lo = cap
				}
				hAll[k] = cap
				lAll[k] = lo
			}
		}
		for k := range es {
			h := hAll[k]
			l := lAll[k]
			if h > edges[es[k]].WeightG {
				h = edges[es[k]].WeightG
			}
			if l > h {
				l = h
			}
			if l < 0 {
				l = 0
			}
			edges[es[k]].LowerG = l
			edges[es[k]].UpperG = h
			to := edges[es[k]].To
			ni := infl[to]
			if ni == nil {
				ni = &interval{}
				infl[to] = ni
			}
			ni.lo += l
			ni.hi += h
			// Dominant incoming edge determines the displayed route: a node reached
			// by two paths shows the larger inflow, and both flows are summed above.
			if !hasPrev[to] || h > edges[prevEdge[to]].UpperG ||
				(h == edges[prevEdge[to]].UpperG && edges[es[k]].MovementID >= edges[prevEdge[to]].MovementID) {
				prevEdge[to] = es[k]
				hasPrev[to] = true
			}
		}
	}

	// Endpoints and buckets.
	pathCache := map[int64][]RecallPathStep{}
	var buildPath func(id int64) []RecallPathStep
	buildPath = func(id int64) []RecallPathStep {
		if p, ok := pathCache[id]; ok {
			return p
		}
		steps := []RecallPathStep{}
		cur := id
		for depth := 0; depth < 64; depth++ {
			if !hasPrev[cur] {
				break
			}
			e := &edges[prevEdge[cur]]
			// Depth-first chain reversed later.
			steps = append(steps, RecallPathStep{
				MovementID: e.MovementID, MovementRef: e.Ref,
				FromLotKey: e.FromKey, ToLotKey: e.ToKey,
				EdgeWeightG: e.WeightG, Basis: e.Basis,
				LowerG: e.LowerG, UpperG: e.UpperG,
			})
			cur = e.From
			if p, ok := pathCache[cur]; ok {
				steps = append(steps, p...)
				break
			}
		}
		for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
			steps[i], steps[j] = steps[j], steps[i]
		}
		pathCache[id] = steps
		return steps
	}

	nodeIDs := make([]int64, 0, len(nodesSet))
	for id := range nodesSet {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool { return nodeIDs[i] < nodeIDs[j] })
	for _, id := range nodeIDs {
		lot := lots[id]
		if lot.WeightG <= 0 {
			continue
		}
		iv := infl[id]
		if iv == nil || iv.hi <= 0 {
			continue
		}
		lo, hi := iv.lo, iv.hi
		if lo > hi {
			lo = hi
		}
		rec := received[id]
		if hi > rec {
			out.CapViolations++
			hi = rec
		}
		if lo > hi {
			lo = hi
		}
		bucket := "on_hand"
		switch lot.PileKind {
		case "production":
			bucket = "production"
		case "shipped":
			bucket = "shipped"
		case "loss":
			bucket = "consumed"
		}
		basis := "exact"
		if lo != hi {
			basis = "bounded"
		}
		node := RecallNode{
			LotID: id, LotKey: lot.Key, PileName: lot.PileName, PileKind: lot.PileKind,
			State: lot.State, Bucket: bucket, ReceivedG: rec, WeightG: lot.WeightG,
			LowerG: lo, UpperG: hi, Basis: basis, Route: buildPath(id),
		}
		out.Nodes = append(out.Nodes, node)
		switch bucket {
		case "on_hand":
			out.OnHandG += hi
			out.OnHandLowerG += lo
		case "consumed":
			out.ConsumedG += hi
			out.ConsumedLowerG += lo
			out.LossG += hi
			out.LossLowerG += lo
		case "production":
			out.ProductionG += hi
			out.ProductionLowerG += lo
		case "shipped":
			out.ShippedG += hi
			out.ShippedLowerG += lo
		}
		out.LowerG += lo
		out.UpperG += hi
	}
	out.UnrecoverableG = out.ProductionG + out.ShippedG + out.ConsumedG
	// Node upper bounds are marginal: "up to X might be here" told separately for
	// each destination cannot all be maximal at once, because every branch draws
	// on the same source truck. The honest whole-truck statement is:
	//   lower = Σ forced node minimums (material that CANNOT be elsewhere);
	//   upper = min(Σ node upper, the truck's total intake) — nothing can come
	//           from outside the source truck.
	// Bucket uppers are then scaled down proportionally so the four legs reconcile
	// to that capped total instead of summing to a number above the intake.
	if out.SourceIntakeG > 0 && out.UpperG > out.SourceIntakeG {
		out.UpperG = out.SourceIntakeG
	}
	if out.LowerG > out.UpperG {
		out.LowerG = out.UpperG
	}
	bucketHi := []*int64{&out.OnHandG, &out.ConsumedG, &out.ProductionG, &out.ShippedG}
	bucketLo := []*int64{&out.OnHandLowerG, &out.ConsumedLowerG, &out.ProductionLowerG, &out.ShippedLowerG}
	var hiSum int64
	for _, p := range bucketHi {
		hiSum += *p
	}
	if hiSum > out.UpperG {
		caps := make([]int64, len(bucketHi))
		for i := range bucketHi {
			caps[i] = *bucketHi[i]
		}
		scaled := scaleTo(caps, out.UpperG)
		for i := range bucketHi {
			v := scaled[i]
			if v < *bucketLo[i] {
				v = *bucketLo[i]
			}
			*bucketHi[i] = v
		}
		// Fix any rounding overshoot deterministically, never below the lower bound.
		var newSum int64
		for _, p := range bucketHi {
			newSum += *p
		}
		for newSum > out.UpperG {
			progress := false
			for i := range bucketHi {
				if newSum <= out.UpperG {
					break
				}
				if *bucketHi[i] > *bucketLo[i] {
					*bucketHi[i]--
					newSum--
					progress = true
				}
			}
			if !progress {
				break
			}
		}
	}
	out.LossG, out.LossLowerG = out.ConsumedG, out.ConsumedLowerG
	out.CheckedG = out.OnHandG + out.ConsumedG + out.ProductionG + out.ShippedG
	out.GapG = out.SourceIntakeG - out.CheckedG
	if out.GapG < 0 {
		out.GapG = 0
	}
	out.OnHandComponentMg = num.MulDiv(out.OnHandG, ppmHi, 1_000_000)
	out.ConsumedComponentMg = num.MulDiv(out.ConsumedG, ppmHi, 1_000_000)
	out.ProductionComponentMg = num.MulDiv(out.ProductionG, ppmHi, 1_000_000)
	out.ShippedComponentMg = num.MulDiv(out.ShippedG, ppmHi, 1_000_000)
	out.LossComponentMg = out.ConsumedComponentMg
	out.LowerComponentMg = num.MulDiv(out.LowerG, ppmLo, 1_000_000)
	out.UpperComponentMg = num.MulDiv(out.UpperG, ppmHi, 1_000_000)
	out.Exact = out.LowerG == out.UpperG && ppmLo == ppmHi

	// The source truck's origin ledger must still carry every material unit the
	// propagation accounted for; if the ledger has less than the truck's intake,
	// some material is unaccounted for (a real cap violation, not a rounding tail).
	if err := q.QueryRow(ctx, `select coalesce(sum(weight_g),0) from rt.lot_origins where trip_id=$1`, tripID).
		Scan(&out.LedgerG); err != nil {
		return out, err
	}
	if out.SourceIntakeG > 0 && out.LedgerG > out.SourceIntakeG {
		out.CapViolations++
	}
	return out, nil
}

// scaleTo allocates total across raw capacities, proportional to raw, summing
// exactly to total and never exceeding any entry's capacity.
func scaleTo(raw []int64, total int64) []int64 {
	out := make([]int64, len(raw))
	if total <= 0 {
		return out
	}
	var sum int64
	for _, v := range raw {
		if v > 0 {
			sum += v
		}
	}
	if sum <= 0 {
		return out
	}
	if total > sum {
		total = sum
	}
	for i, v := range raw {
		out[i] = num.MulDiv(v, total, sum)
	}
	var assigned int64
	for _, v := range out {
		assigned += v
	}
	left := total - assigned
	// Give the remainder to entries with room, in capacity order, one unit each.
	for pass := 0; left > 0; pass++ {
		progress := false
		for i := range raw {
			if left <= 0 {
				break
			}
			if out[i] < raw[i] {
				out[i]++
				left--
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func currentEpoch(ctx context.Context, q querier) (int64, error) {
	var e int64
	err := q.QueryRow(ctx, `select epoch from rt.stock_epoch where id=1`).Scan(&e)
	return e, err
}

func bumpEpoch(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `update rt.stock_epoch set epoch=epoch+1 where id=1`)
	return err
}

// lockStockWorld serialises inventory mutations against investigation snapshots
// and isolation approval. Transfers, intake, investigations and isolation
// actions all take this advisory lock, so "read the stock, decide, then write"
// never interleaves: a snapshot cannot be computed mid-transfer, and an approval
// cannot slip in a hold between a transfer's balance check and its commit.
func lockStockWorld(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('rt.stock-world'))`)
	return err
}
