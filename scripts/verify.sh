#!/usr/bin/env bash
# 端到端验证脚本：不依赖真实仪器，使用项目自带样例 + 数据库断言。
# 用法： ./scripts/verify.sh            （使用 backend 默认 DSN）
#        BASE=http://127.0.0.1:8080 ./scripts/verify.sh
set -uo pipefail
cd "$(dirname "$0")/.."
BASE="${BASE:-http://127.0.0.1:8080}"
DSN="${DATABASE_URL:-postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable}"
PSQL_BIN="${PSQL_BIN:-/opt/postgresql-18/bin/psql}"
GO_BIN="${GO_BIN:-/opt/go/bin/go}"
fail=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }
q() { "$PSQL_BIN" "$DSN" -tAc "$1"; }
tok() { curl -s -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$1\",\"password\":\"$2\"}" | jq -r .token; }

echo "== 0. 服务可用性 + 迁移 + 重置样例数据 =="
if ! curl -sf "$BASE/api/health" >/dev/null; then
  echo "ERROR: $BASE 不可达，请先启动服务（见 README：一键启动 / 本地启动）"; exit 1
fi
( cd backend && "$GO_BIN" run ./cmd/seed ) || { echo "seed failed"; exit 1; }

echo "== 1. 逐车批次独立 =="
n=$(q "select count(*) from rt.trips where supplier_id=1 and arrival_at::date=(select max(arrival_at::date) from rt.trips)")
d=$(q "select count(distinct batch_no) from rt.trips")
[ "$n" -ge 2 ] && [ "$d" -eq "$(q 'select count(*) from rt.trips')" ] && pass "同日同供应商 $n 车，批次号互不相同" || bad "批次号不唯一"

echo "== 2. 来源账 = 批次余额 =="
gap=$(q "select count(*) from (select l.id, l.weight_g, coalesce(sum(o.weight_g),0) ow, l.metal_mg, coalesce(sum(o.metal_mg),0) om from rt.inventory_lots l left join rt.lot_origins o on o.lot_id=l.id group by l.id,l.weight_g,l.metal_mg) x where weight_g<>ow or metal_mg<>om")
[ "$gap" -eq 0 ] && pass "所有批次 来源重量/金属 == 余额" || bad "$gap 个批次来源账与余额不符"

echo "== 3. 转移单守恒 out = in + loss =="
gap=$(q "select count(*) from (select m.id, coalesce(sum(tl.weight_g) filter (where tl.direction='out'),0) o, coalesce(sum(tl.weight_g) filter (where tl.direction='in'),0) i, coalesce(sum(tl.weight_g) filter (where tl.direction='loss'),0) l from rt.movements m join rt.transfer_lines tl on tl.movement_id=m.id group by m.id) x where o>0 and o<>i+l")
[ "$gap" -eq 0 ] && pass "每张转移单 转出 = 转入 + 明确损耗" || bad "$gap 张转移单数量不守恒"

echo "== 4. 隔离料未进入可用堆 =="
n=$(q "select count(*) from rt.lot_origins o join rt.inventory_lots l on l.id=o.lot_id join rt.piles p on p.id=l.pile_id join rt.grade_approvals g on g.trip_id=o.trip_id where p.kind='usable' and g.isolated")
[ "$n" -eq 0 ] && pass "可用堆无隔离料来源" || bad "$n 条隔离料来源出现在可用堆"

echo "== 5. 应付款版本：原单保留 + 差额 =="
v=$(q "select count(*) from rt.payables where trip_id=1")
cur=$(q "select amount_cents from rt.payables where trip_id=1 and is_current")
old=$(q "select amount_cents from rt.payables where trip_id=1 and version=1")
[ "$v" -ge 2 ] && [ "$cur" -lt "$old" ] && pass "车次1 有 $v 个版本，复检后金额 $old → $cur 分（下调）" || bad "版本保留或差额异常 (v=$v cur=$cur old=$old)"

echo "== 6. 重放同一转移单不重复移动 =="
before=$(q "select coalesce(sum(weight_g),0) from rt.inventory_lots where pile_id=(select id from rt.piles where code='SHIP-1')")
YARD=$(tok yard yard123)
curl -s -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
  -d '{"external_ref":"MX-2601-001","kind":"mix","mode":"uniform_share","sources":[{"lot_id":1,"weight_g":1}],"destinations":[{"pile_id":1,"weight_g":1}]}' >/dev/null
after=$(q "select coalesce(sum(weight_g),0) from rt.inventory_lots where pile_id=(select id from rt.piles where code='SHIP-1')")
[ "$before" = "$after" ] && pass "重放 MX-2601-001 后发货区总量不变 ($after g)" || bad "重放导致材料移动 $before -> $after"

echo "== 7. 超库存与隔离混料被拒绝 =="
ISO=$(q "select id from rt.inventory_lots where pile_id=(select id from rt.piles where code='ISO-1') limit 1")
ISOW=$(q "select weight_g from rt.inventory_lots where id=$ISO")
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
  -d "{\"external_ref\":\"VERIFY-OVER\",\"kind\":\"split\",\"mode\":\"uniform_share\",\"sources\":[{\"lot_id\":$ISO,\"weight_g\":$((ISOW+1))}],\"destinations\":[{\"pile_id\":1,\"weight_g\":$((ISOW+1))}]}")
[ "$code" = "409" ] && pass "超库存被拒绝 (HTTP $code)" || bad "超库存未被拒绝 (HTTP $code)"
U1=$(q "select id from rt.piles where code='USABLE-1'")
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
  -d "{\"external_ref\":\"VERIFY-ISO\",\"kind\":\"mix\",\"mode\":\"uniform_share\",\"sources\":[{\"lot_id\":$ISO,\"weight_g\":$ISOW}],\"destinations\":[{\"pile_id\":$U1,\"weight_g\":$ISOW}]}")
[ "$code" = "409" ] && pass "隔离料混入可用堆被拒绝 (HTTP $code)" || bad "隔离料混入未被拒绝 (HTTP $code)"

echo "== 8. 缺归属化验被拒收 =="
LAB=$(tok lab lab123)
out=$(curl -s -X POST "$BASE/api/ingest/assays" -H "Authorization: Bearer $LAB" -H 'Content-Type: application/json' \
  -d '[{"sample_no":"NO-TRIP-1","position":"车厢中部","seal_no":"X","moisture_pct":3,"impurity_pct":1,"impurity_basis":"dry","metal_pct":60,"metal_basis":"dry"}]')
echo "$out" | jq -e '.items[0].outcome=="rejected"' >/dev/null && pass "缺 trip_key 的化验被拒收" || bad "缺归属化验未被拒收: $out"

echo "== 9. 磅秤报文重放幂等 =="
YARD=$(tok yard yard123)
out=$(curl -s -X POST "$BASE/api/ingest/scale" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' -d @samples/scale/2601-trucks.json)
acc=$(echo "$out" | jq -r .accepted); dup=$(echo "$out" | jq -r .duplicate)
[ "$acc" -eq 0 ] && [ "$dup" -eq 7 ] && pass "已导入的磅单再次投递全部判重 ($dup)" || bad "磅单重放行为异常 acc=$acc dup=$dup"

echo "== 10. 干燥损耗不减金属 =="
dry_metal=$(q "select coalesce(sum(metal_mg),0) from rt.loss_attribution where loss_class='drying'")
[ "$dry_metal" = "0" ] && pass "所有干燥损耗的金属扣减为 0" || bad "干燥损耗扣了金属 $dry_metal mg"

echo "== 11. 合同基准：干基与湿基公式结果不同 =="
FIN=$(tok finance finance123)
f=$(curl -s "$BASE/api/formulas" -H "Authorization: Bearer $FIN")
echo "$f" | jq -e 'length==2 and (map(.code)|index("DRY-IMP-METAL")!=null) and (map(.code)|index("WET-NET-METAL")!=null)' >/dev/null \
  && pass "登记了两个独立公式（干基/湿基）" || bad "公式登记异常: $f"

echo "== 12. 全场合计守恒 =="
t=$(curl -s "$BASE/api/reconciliation" -H "Authorization: Bearer $FIN")
echo "$t" | jq -e '.totals.gap_g==0 and .totals.gap_metal_mg==0' >/dev/null \
  && pass "重量尾差与金属尾差均为 0" || bad "存在尾差: $(echo "$t" | jq -c .totals)"

echo "== 13. 排查：两条路径汇合不重复计数 =="
QTOK=$(tok quality quality123)
inv=$(curl -s "$BASE/api/quality/investigations/1" -H "Authorization: Bearer $QTOK")
echo "$inv" | jq -e '.node_count>0 and .cap_violations==0' >/dev/null \
  && pass "所有节点影响量均未超过其历史接收量" || bad "存在节点影响量超过历史接收量"
up=$(echo "$inv" | jq -r .upper_g)
src=$(echo "$inv" | jq -r .source_intake_g)
[ "$up" -le "$src" ] && pass "受影响量上界 $up g ≤ 源车接收量 $src g（汇合未重复计数）" || bad "上界超过源车接收量 ($up > $src)"

echo "== 14. 分拣未知：给出下界/上界且不伪装精确 =="
lo=$(echo "$inv" | jq -r .lower_g)
echo "$inv" | jq -e '.exact==false and (.lower_g < .upper_g)' >/dev/null \
  && pass "排查结果标记为区间：$lo g – $up g" || bad "未知分拣未给出区间"

echo "== 15. 存量/在制/已发分别列出且核对 =="
echo "$inv" | jq -e '.on_hand_g>0 and .shipped_g>0 and .production_g>0 and .unrecoverable_g>0 and .reconcile.gap_g==0 and .reconcile.balanced==true' >/dev/null \
  && pass "存量/在制/已发分列，处置合计与源车可解释范围核对一致" || bad "去向分列或核对异常: $(echo "$inv" | jq -c .reconcile)"

echo "== 16. 隔离申请展示依赖存量与在途动作 =="
ord=$(curl -s "$BASE/api/quality/isolation/1" -H "Authorization: Bearer $QTOK")
echo "$ord" | jq -e '.status=="pending" or .status=="active"' >/dev/null || bad "处置单 1 缺失"
echo "$ord" | jq -e '(.depending|length)>0 and (.in_flight|length)>0' >/dev/null \
  && pass "处置单展示依赖存量 $(echo "$ord"|jq -r '.depending|length') 项、在途动作 $(echo "$ord"|jq -r '.in_flight|length') 项" \
  || bad "处置单未展示依赖存量或在途动作"

echo "== 17. 批准隔离后阻断新领料 =="
curl -s -X POST "$BASE/api/quality/isolation/1/approve" -H "Authorization: Bearer $QTOK" > /tmp/iso_approve.json 2>/dev/null
status=$(jq -r '.status // empty' /tmp/iso_approve.json 2>/dev/null)
held=$(q "select count(*) from rt.lot_holds where order_id=1 and released=false")
if [ "$status" = "active" ] || [ "$held" -gt 0 ]; then
  pass "隔离生效，阻断 $held 个在库批次"
else
  bad "隔离未生效: status=$status held=$held"
fi
heldlot=$(q "select lot_id from rt.lot_holds where released=false limit 1")
if [ -n "$heldlot" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
    -d "{\"external_ref\":\"VERIFY-HELD\",\"kind\":\"split\",\"mode\":\"uniform_share\",\"sources\":[{\"lot_id\":$heldlot,\"weight_g\":1000}],\"destinations\":[{\"pile_id\":$(q "select id from rt.piles where code='RECV-A'"),\"weight_g\":1000,\"grade_code\":\"CU-BRIGHT\"}]}"})
  [ "$code" = "409" ] && pass "被阻断批次的新领料被拒绝 (HTTP $code)" || bad "被阻断批次仍可领料 (HTTP $code)"
fi

echo "== 18. 部分复核：剩余继续阻断 =="
first=$(q "select lot_id from rt.lot_holds where order_id=1 and released=false order by lot_id limit 1")
curl -s -X POST "$BASE/api/quality/isolation/1/review" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d "{\"scope_lot_ids\":[$first],\"scope_note\":\"仅复核一个批次\"}" >/dev/null
left=$(q "select count(*) from rt.lot_holds where order_id=1 and released=false")
[ "$left" -ge 1 ] && pass "只复核部分后仍有 $left 个批次阻断" || bad "部分复核错误释放了全部"

echo "== 19. 事件重试不重复扩大持有 =="
before=$(q "select count(*) from rt.lot_holds where event_id=1")
curl -s -X POST "$BASE/api/quality/events" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d '{"event_no":"QEV-2601-001","kind":"risk_range","trip_id":4,"param":"impurity_ppm","min_ppm":20000,"max_ppm":32000,"basis":"customer_complaint","note":"重试"}' >/dev/null
after=$(q "select count(*) from rt.lot_holds where event_id=1")
[ "$before" = "$after" ] && pass "重试同一事件号未新增持有（$after）" || bad "事件重试扩大了持有 $before -> $after"

echo "== 20. 多起事件重叠必须全部复核 =="
E2=$(curl -s -X POST "$BASE/api/quality/events" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d '{"event_no":"QEV-2601-002","kind":"composition","trip_id":5,"param":"metal_ppm","min_ppm":580000,"max_ppm":580000,"basis":"manual","note":"重叠事件"}' | jq -r .id)
INV2=$(curl -s -X POST "$BASE/api/quality/events/$E2/investigate" -H "Authorization: Bearer $QTOK" | jq -r .id)
O2=$(curl -s -X POST "$BASE/api/quality/isolation" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d "{\"order_no\":\"ISO-2601-VERIFY-2\",\"event_id\":$E2,\"investigation_id\":$INV2}" | jq -r .id)
curl -s -X POST "$BASE/api/quality/isolation/$O2/approve" -H "Authorization: Bearer $QTOK" >/dev/null
shared=$(q "select lot_id from rt.lot_holds where event_id=$E2 and released=false limit 1")
if [ -n "$shared" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
    -d "{\"external_ref\":\"VERIFY-OVERLAP\",\"kind\":\"split\",\"mode\":\"uniform_share\",\"sources\":[{\"lot_id\":$shared,\"weight_g\":1000}],\"destinations\":[{\"pile_id\":$(q "select id from rt.piles where code='RECV-A'"),\"weight_g\":1000,\"grade_code\":\"CU-BRIGHT\"}]}"})
  [ "$code" = "409" ] && pass "重叠事件的批次仍被阻断 (HTTP $code)" || bad "重叠事件未阻断 (HTTP $code)"
fi

echo "== 21. 旧排查结果在新转移后不得直接批准 =="
E3=$(curl -s -X POST "$BASE/api/quality/events" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d '{"event_no":"QEV-2601-STALE","kind":"composition","trip_id":4,"param":"impurity_ppm","min_ppm":20000,"max_ppm":20000,"basis":"manual","note":"过期快照测试"}' | jq -r .id)
INV3=$(curl -s -X POST "$BASE/api/quality/events/$E3/investigate" -H "Authorization: Bearer $QTOK" | jq -r .id)
O3=$(curl -s -X POST "$BASE/api/quality/isolation" -H "Authorization: Bearer $QTOK" -H 'Content-Type: application/json' \
  -d "{\"order_no\":\"ISO-2601-STALE\",\"event_id\":$E3,\"investigation_id\":$INV3}" | jq -r .id)
free=$(q "select l.id from rt.inventory_lots l join rt.piles p on p.id=l.pile_id where p.kind='isolated' and l.weight_g>1000 limit 1")
curl -s -X POST "$BASE/api/transfers" -H "Authorization: Bearer $YARD" -H 'Content-Type: application/json' \
  -d "{\"external_ref\":\"VERIFY-EPOCH\",\"kind\":\"split\",\"mode\":\"uniform_share\",\"sources\":[{\"lot_id\":$free,\"weight_g\":1000}],\"destinations\":[{\"pile_id\":$(q "select id from rt.piles where code='ISO-1'"),\"weight_g\":1000,\"grade_code\":\"CU-DIRTY\"}]}" >/dev/null
resp=$(curl -s -X POST "$BASE/api/quality/isolation/$O3/approve" -H "Authorization: Bearer $QTOK")
echo "$resp" | jq -e '.error != null' >/dev/null \
  && pass "新转移后旧排查快照被拒绝批准" || bad "旧排查快照仍可直接批准: $resp"

echo
if [ "$fail" -eq 0 ]; then echo "ALL CHECKS PASSED"; else echo "SOME CHECKS FAILED"; fi
exit "$fail"
