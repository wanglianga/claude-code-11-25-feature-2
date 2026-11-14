<script setup>
import { ref, onMounted, computed } from 'vue'
import { api, getUser, kg } from '../api/client.js'

const user = getUser()
const canQuality = computed(() => ['quality', 'admin'].includes(user?.role))
const events = ref([])
const orders = ref([])
const selected = ref(null)          // selected event
const inv = ref(null)               // latest investigation snapshot
const order = ref(null)             // selected isolation order
const error = ref('')
const notice = ref('')
const busy = ref(false)

const paramLabel = { metal_ppm: '金属比例', impurity_ppm: '非金属杂物', moisture_ppm: '水分' }
const bucketLabel = { on_hand: '在库存量', production: '在制（已投炉）', shipped: '已发产品', consumed: '已耗用/损耗' }
const fmt = (g) => kg(Math.abs(Number(g || 0)))
const range = (lo, hi) => (Number(lo) === Number(hi) ? fmt(lo) + ' kg' : `${fmt(lo)} – ${fmt(hi)} kg`)

async function load() {
  error.value = ''
  try {
    events.value = await api.get('/api/quality/events')
    orders.value = await api.get('/api/quality/isolation')
    if (events.value.length && !selected.value) await openEvent(events.value[0])
  } catch (e) { error.value = e.message }
}
onMounted(load)

async function openEvent(ev) {
  selected.value = ev
  order.value = null
  inv.value = null
  try {
    const latest = ev.latest_investigation_id
    if (latest) inv.value = await api.get(`/api/quality/investigations/${latest}`)
    const own = orders.value.find(o => o.event_id === ev.id)
    if (own) order.value = await api.get(`/api/quality/isolation/${own.id}`)
  } catch (e) { error.value = e.message }
}

async function investigate() {
  if (!selected.value) return
  await act(async () => {
    inv.value = await api.post(`/api/quality/events/${selected.value.id}/investigate`, {})
    await refreshEvents()
    return `排查快照 #${inv.value.seq} 生成：下界 ${fmt(inv.value.lower_g)} kg，上界 ${fmt(inv.value.upper_g)} kg`
  })
}

async function applyIsolation() {
  if (!inv.value) { error.value = '请先生成排查快照'; return }
  await act(async () => {
    order.value = await api.post('/api/quality/isolation', {
      order_no: `ISO-${selected.value.event_no}-${Date.now()}`,
      event_id: selected.value.id,
      investigation_id: inv.value.id,
      scope_note: '按最新排查快照申请隔离',
    })
    await load()
    return '隔离申请已提交（尚未生效）：请核对依赖存量与在途动作后批准'
  })
}

async function approve() {
  await act(async () => {
    order.value = await api.post(`/api/quality/isolation/${order.value.id}/approve`, {})
    await load()
    return `隔离已生效，阻断 ${order.value.holds.filter(h => !h.released).length} 个批次`
  })
}

async function reviewFull() {
  await act(async () => {
    order.value = await api.post(`/api/quality/isolation/${order.value.id}/review`, {
      scope_note: '复核全部范围（抽检合格 / 已单独处置）',
    })
    await load()
    return '已提交全范围复核'
  })
}

async function reviewOne(lotId) {
  await act(async () => {
    order.value = await api.post(`/api/quality/isolation/${order.value.id}/review`, {
      scope_lot_ids: [lotId], scope_note: '仅复核该批次，其余继续阻断',
    })
    await load()
    return '已复核单个批次，其余持有继续阻断'
  })
}

async function refreshEvents() {
  events.value = await api.get('/api/quality/events')
  orders.value = await api.get('/api/quality/isolation')
}

async function act(fn) {
  error.value = ''; notice.value = ''; busy.value = true
  try { notice.value = (await fn()) || '' }
  catch (e) { error.value = e.message }
  finally { busy.value = false }
}

const groups = computed(() => {
  const g = { on_hand: [], production: [], shipped: [], consumed: [] }
  for (const n of (inv.value?.nodes || [])) (g[n.bucket] || g.on_hand).push(n)
  return g
})

const reconcileOk = computed(() => inv.value && inv.value.reconcile &&
  inv.value.reconcile.gap_g === 0 && inv.value.reconcile.nodes_within_receipts)

const openOrders = computed(() => orders.value.filter(o => o.status === 'active' || o.status === 'pending'))
</script>

<template>
  <div>
    <h1>质量排查与隔离处置</h1>
    <p class="lead">
      从某车问题出发，沿带数量的转移关系传播到剩余料堆、在制炉次与已发产品；汇合节点按来源流量归并，
      分拣未知时给出受影响量上下界，不伪装精确。已发、在制与已耗用会明确标为“无法追回”。
    </p>
    <div v-if="error" class="err">{{ error }}</div>
    <div v-if="notice" class="ok">{{ notice }}</div>

    <div class="panel">
      <div class="row">
        <h2 style="margin:0">质量事件</h2>
        <span class="spacer"></span>
        <button class="ghost small" @click="load">刷新</button>
      </div>
      <table v-if="events.length">
        <thead>
          <tr><th>事件号</th><th>车次</th><th>类型</th><th>参数 / 范围</th><th>状态</th><th>排查版本</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-for="e in events" :key="e.id">
            <td class="mono">{{ e.event_no }}</td>
            <td class="mono">{{ e.batch_no }}</td>
            <td><span class="badge gray">{{ e.kind === 'composition' ? '具体成分' : '风险范围' }}</span></td>
            <td>{{ paramLabel[e.param] || e.param }}
              <span class="muted"> {{ (e.min_ppm / 10000).toFixed(2) }}%
                <template v-if="e.max_ppm !== e.min_ppm"> – {{ (e.max_ppm / 10000).toFixed(2) }}%</template>
              </span>
            </td>
            <td><span class="badge" :class="{ warn: e.status === 'open', gray: e.status === 'closed' }">{{ e.status }}</span></td>
            <td>{{ e.latest_seq ? ('#' + e.latest_seq) : '—' }}</td>
            <td>
              <button class="ghost small" @click="openEvent(e)">查看</button>
              <button v-if="canQuality" class="ghost small" :disabled="busy" @click="openEvent(e).then(investigate)">重新排查</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="muted">暂无质量事件。</p>
    </div>

    <template v-if="selected && inv">
      <div class="panel">
        <div class="row">
          <h2 style="margin:0">排查快照 #{{ inv.seq }} · {{ selected.event_no }}</h2>
          <span class="badge" :class="inv.exact ? '' : 'warn'">
            {{ inv.exact ? '数量精确' : '分拣未知 · 区间' }}
          </span>
          <span v-if="inv.stale" class="badge danger">已过期：排查后又发生转移，需重新排查</span>
          <span class="spacer"></span>
          <button v-if="canQuality" class="subtle small" :disabled="busy" @click="investigate">重新排查</button>
        </div>
        <p class="muted" style="font-size:12px">
          参数 {{ paramLabel[inv.param] || inv.param }} 风险区间
          {{ (inv.lower_ppm / 10000).toFixed(2) }}% – {{ (inv.upper_ppm / 10000).toFixed(2) }}% ·
          库存序号 {{ inv.stock_epoch }}
        </p>

        <div class="statgrid">
          <div class="stat">
            <div class="label">在库存量（可隔离）</div>
            <div class="value">{{ range(inv.on_hand_lower_g, inv.on_hand_g) }}</div>
            <div class="hint">阻断新的领料</div>
          </div>
          <div class="stat">
            <div class="label">在制（已投炉）</div>
            <div class="value">{{ range(inv.production_lower_g, inv.production_g) }}</div>
            <div class="hint">已无法回滚</div>
          </div>
          <div class="stat">
            <div class="label">已发产品</div>
            <div class="value">{{ range(inv.shipped_lower_g, inv.shipped_g) }}</div>
            <div class="hint">只能继续追查下游</div>
          </div>
          <div class="stat">
            <div class="label">已耗用 / 损耗</div>
            <div class="value">{{ range(inv.loss_lower_g, inv.loss_g) }}</div>
            <div class="hint">无法追回</div>
          </div>
        </div>

        <dl class="kv" style="margin-top:6px">
          <dt>受影响总量</dt><dd class="num">{{ range(inv.lower_g, inv.upper_g) }}</dd>
          <dt>源车历史接收量</dt><dd class="num">{{ fmt(inv.source_intake_g) }} kg</dd>
          <dt>处置核对</dt>
          <dd class="num">
            <span :style="{ color: reconcileOk ? 'var(--accent)' : 'var(--danger)' }">
              {{ reconcileOk ? '各去向合计 = 源车接收量，节点影响量均未超过历史接收量' : '存在尾差或节点超过接收量，请检查' }}
            </span>
            <span class="muted">（合计 {{ fmt(inv.checked_g) }} kg，尾差 {{ fmt(inv.reconcile.gap_g) }} kg）</span>
          </dd>
          <dt>无法追回合计</dt><dd class="num">{{ fmt(inv.unrecoverable_g) }} kg</dd>
        </dl>
      </div>

      <div class="panel">
        <h2>处置名单（按去向）</h2>
        <div v-for="(list, key) in groups" :key="key">
          <h3>
            {{ bucketLabel[key] }}
            <span class="badge" :class="{ danger: key !== 'on_hand' }">{{ list.length }}</span>
          </h3>
          <table v-if="list.length">
            <thead>
              <tr><th>批次节点</th><th>堆位</th><th class="num">影响量（kg）</th><th class="num">历史接收</th>
                <th>依据</th><th>完整路线</th><th></th></tr>
            </thead>
            <tbody>
              <tr v-for="n in list" :key="n.lot_id">
                <td class="mono">{{ n.lot_key }}</td>
                <td>{{ n.pile_name }}</td>
                <td class="num">{{ range(n.lower_g, n.upper_g) }}</td>
                <td class="num muted">{{ fmt(n.received_g) }}</td>
                <td><span class="badge" :class="n.basis === 'bounded' ? 'warn' : 'gray'">
                  {{ n.basis === 'bounded' ? '上下界' : '精确' }}</span></td>
                <td>
                  <details>
                    <summary class="muted" style="cursor:pointer;font-size:12px">
                      {{ n.route.length }} 步
                    </summary>
                    <table style="margin-top:6px">
                      <thead><tr><th>转移单</th><th>从</th><th>到</th><th class="num">流量 kg</th><th class="num">承载 kg</th><th>依据</th></tr></thead>
                      <tbody>
                        <tr v-for="(st, i) in n.route" :key="i">
                          <td class="mono">{{ st.movement_ref }}</td>
                          <td class="mono">{{ st.from_lot_key }}</td>
                          <td class="mono">{{ st.to_lot_key }}</td>
                          <td class="num">{{ fmt(st.edge_weight_g) }}</td>
                          <td class="num">{{ range(st.lower_g, st.upper_g) }}</td>
                          <td><span class="badge gray">{{ st.basis }}</span></td>
                        </tr>
                      </tbody>
                    </table>
                  </details>
                </td>
                <td>
                  <router-link :to="`/stock`" class="muted" style="font-size:12px">料堆</router-link>
                </td>
              </tr>
            </tbody>
          </table>
          <p v-else class="muted" style="font-size:12px">无</p>
        </div>
      </div>

      <div class="panel">
        <h2>隔离处置单</h2>
        <div v-if="order">
          <div class="row" style="margin-bottom:10px">
            <span class="mono">{{ order.order_no }}</span>
            <span class="badge" :class="{ warn: order.status === 'pending' || order.status === 'active',
              gray: order.status === 'closed' }">{{ order.status }}</span>
            <span v-if="order.stale" class="badge danger">排查快照已过期</span>
            <span class="spacer"></span>
            <button v-if="canQuality && order.status === 'pending'" :disabled="busy" @click="approve">批准隔离</button>
            <button v-if="canQuality && order.status === 'active'" class="ghost small" :disabled="busy" @click="reviewFull">
              全范围复核</button>
          </div>
          <div v-if="order.status === 'pending'" class="warnbox" style="font-size:12px">
            待批准。批准前请核对下列“依赖的当前存量”和“正在执行动作”。批准会阻断这些批次的新领料；
            已完成的动作保留，并继续追查下游，不会回滚成料还在场。
          </div>

          <h3>依赖的当前存量（{{ order.depending.length }}）</h3>
          <table v-if="order.depending.length">
            <thead><tr><th>批次</th><th>堆位</th><th class="num">当前余额 kg</th><th class="num">命中量 kg</th>
              <th>依据</th><th>阻断</th></tr></thead>
            <tbody>
              <tr v-for="h in order.depending" :key="h.id">
                <td class="mono">{{ h.lot_key }}</td>
                <td>{{ h.pile_name }}</td>
                <td class="num">{{ fmt(h.current_g) }}</td>
                <td class="num">{{ range(h.hit_lower_g, h.hit_upper_g) }}</td>
                <td><span class="badge gray">{{ h.basis }}</span></td>
                <td>
                  <span class="badge" :class="h.released ? 'gray' : 'danger'">{{ h.released ? '已复核释放' : '阻断中' }}</span>
                  <button v-if="canQuality && order.status === 'active' && !h.released"
                    class="ghost small" style="margin-left:6px;font-size:11px;padding:3px 8px"
                    :disabled="busy" @click="reviewOne(h.lot_id)">复核此批次</button>
                </td>
              </tr>
            </tbody>
          </table>
          <p v-else class="muted" style="font-size:12px">
            待批准：批准时按排查快照命中所有在库节点并写入阻断。
          </p>

          <h3>正在执行的动作（已应用、不可回滚，继续追查下游）</h3>
          <table v-if="order.in_flight.length">
            <thead><tr><th>转移单</th><th>类型</th><th>去向批次</th><th>去向</th><th class="num">重量 kg</th><th>时间</th></tr></thead>
            <tbody>
              <tr v-for="f in order.in_flight" :key="f.movement_id + '-' + f.lot_key">
                <td class="mono">{{ f.external_ref }}</td>
                <td><span class="badge gray">{{ f.kind }}</span></td>
                <td class="mono">{{ f.lot_key }}</td>
                <td>{{ f.pile_kind === 'production' ? '投炉在制' : f.pile_kind === 'shipped' ? '已发产品' : '损耗' }}</td>
                <td class="num">{{ fmt(f.weight_g) }}</td>
                <td class="muted mono">{{ new Date(f.occurred_at).toLocaleString('zh-CN') }}</td>
              </tr>
            </tbody>
          </table>
          <p v-else class="muted" style="font-size:12px">无在途动作。</p>

          <h3>复核记录</h3>
          <table v-if="order.reviews.length">
            <thead><tr><th>#</th><th>覆盖</th><th>范围</th><th>说明</th><th>时间</th></tr></thead>
            <tbody>
              <tr v-for="r in order.reviews" :key="r.id">
                <td>{{ r.id }}</td>
                <td><span class="badge" :class="r.coverage === 'full' ? '' : 'warn'">
                  {{ r.coverage === 'full' ? '全范围' : '部分' }}</span></td>
                <td class="mono">{{ r.scope_lot_ids.length ? r.scope_lot_ids.join(', ') : '全部持有' }}</td>
                <td class="muted">{{ r.scope_note }}</td>
                <td class="muted mono">{{ new Date(r.reviewed_at).toLocaleString('zh-CN') }}</td>
              </tr>
            </tbody>
          </table>
          <p v-else class="muted" style="font-size:12px">
            尚未复核。复核只覆盖部分时，剩余批次继续阻断；多起事件重叠时必须全部复核才释放。
          </p>
        </div>
        <div v-else>
          <p class="muted" style="font-size:12px">
            该事件尚无处置单。
            <span v-if="canQuality">先重新排查得到最新快照，再申请隔离。</span>
          </p>
          <button v-if="canQuality" :disabled="busy || inv.stale" @click="applyIsolation">申请隔离（基于快照 #{{ inv.seq }}）</button>
          <span v-if="inv.stale" class="muted" style="font-size:12px;margin-left:8px">
            快照已过期，请先重新排查。
          </span>
        </div>
      </div>
    </template>

    <div v-else-if="selected" class="panel">
      <h2>{{ selected.event_no }}</h2>
      <p class="muted">该事件还没有排查快照。</p>
      <button v-if="canQuality" :disabled="busy" @click="investigate">开始排查</button>
    </div>

    <div class="panel" v-if="openOrders.length">
      <h2>生效中的隔离单</h2>
      <table>
        <thead><tr><th>处置单</th><th>事件</th><th>状态</th><th class="num">仍阻断 kg</th><th class="num">持有批次</th></tr></thead>
        <tbody>
          <tr v-for="o in openOrders" :key="o.id">
            <td class="mono">{{ o.order_no }}</td>
            <td class="mono">{{ o.event_no }}</td>
            <td><span class="badge warn">{{ o.status }}</span></td>
            <td class="num">{{ fmt(o.remaining_hold_g) }}</td>
            <td class="num">{{ o.holds.filter(h => !h.released).length }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
