-- 质量事件、下游排查快照、隔离处置。
-- 设计要点：
--   * 排查按“带数量的转移关系”传播，汇合节点按来源流量归并，节点累计影响量不得超过其历史接收量；
--   * 来源分拣依据 unknown 时，重量同时给出受影响下界/上界，绝不伪装精确；
--   * 排查结果携带 stock_epoch：自排查后又发生转移时，旧结论不得直接批准隔离；
--   * 隔离持有按 (事件, 批次) 唯一，事件重试不重复扩大数量；
--   * 多起事件重叠时，同一批次必须所有事件的复核都覆盖才解除阻断。

create table if not exists rt.quality_events (
  id bigserial primary key,
  event_no text not null unique,              -- 幂等键：重试同一事件号返回原事件
  kind text not null check (kind in ('composition','risk_range')),
  trip_id bigint not null references rt.trips(id),
  param text not null check (param in ('metal_ppm','impurity_ppm','moisture_ppm')),
  min_ppm bigint not null check (min_ppm between 0 and 1000000),
  max_ppm bigint not null check (max_ppm between 0 and 1000000),
  basis text not null default 'adopted_assay'
    check (basis in ('adopted_assay','recheck','customer_complaint','manual')),
  note text not null default '',
  reported_by bigint not null references rt.users(id),
  reported_at timestamptz not null default now(),
  status text not null default 'open' check (status in ('open','closed')),
  created_at timestamptz not null default now(),
  check (min_ppm <= max_ppm)
);
create index if not exists quality_events_trip_idx on rt.quality_events(trip_id);

-- 一次排查 = 一个不可变快照。重跑生成新的 investigation，旧结果永久保留。
create table if not exists rt.quality_investigations (
  id bigserial primary key,
  event_id bigint not null references rt.quality_events(id) on delete cascade,
  seq int not null,
  stock_epoch bigint not null,                -- 生成时的库存变更序号
  param text not null,
  lower_ppm bigint not null,                  -- 归一后的受影响参数范围
  upper_ppm bigint not null,
  exact boolean not null,
  -- 四个去向：重量 g；component 为“受影响参数”的质量 mg。
  on_hand_g bigint not null default 0,
  on_hand_lower_g bigint not null default 0,
  on_hand_component_mg bigint not null default 0,
  consumed_g bigint not null default 0,
  consumed_lower_g bigint not null default 0,
  consumed_component_mg bigint not null default 0,
  production_g bigint not null default 0,
  production_lower_g bigint not null default 0,
  production_component_mg bigint not null default 0,
  shipped_g bigint not null default 0,
  shipped_lower_g bigint not null default 0,
  shipped_component_mg bigint not null default 0,
  loss_g bigint not null default 0,
  loss_lower_g bigint not null default 0,
  loss_component_mg bigint not null default 0,
  unrecoverable_g bigint not null default 0,  -- 已发 + 在制 + 已耗用（无法回滚）
  source_intake_g bigint not null default 0,  -- 源车历史接收量（可解释范围上限）
  checked_g bigint not null default 0,        -- 各节点影响量之和
  ledger_g bigint not null default 0,         -- 来源账中该车仍存活的材料合计
  gap_g bigint not null default 0,            -- source_intake - checked
  lower_g bigint not null default 0,          -- 受影响总量下界
  upper_g bigint not null default 0,          -- 受影响总量上界
  lower_component_mg bigint not null default 0,
  upper_component_mg bigint not null default 0,
  cap_violations int not null default 0,      -- 节点影响量超过历史接收量的个数
  node_count int not null default 0,
  detail jsonb not null default '{}'::jsonb,  -- 节点与路线明细快照
  created_by bigint not null references rt.users(id),
  created_at timestamptz not null default now(),
  unique (event_id, seq)
);

-- 库存变更序号：每次成功入库/转移 +1，供排查快照判断“是否已过期”。
create table if not exists rt.stock_epoch (
  id int primary key default 1 check (id = 1),
  epoch bigint not null default 0
);
insert into rt.stock_epoch(id, epoch) values (1, 0) on conflict (id) do nothing;

create table if not exists rt.isolation_orders (
  id bigserial primary key,
  order_no text not null unique,
  event_id bigint not null references rt.quality_events(id),
  investigation_id bigint not null references rt.quality_investigations(id),
  applied_epoch bigint not null,              -- 申请时的库存快照
  approved_epoch bigint,
  status text not null default 'pending'
    check (status in ('pending','active','closed','rejected')),
  scope_note text not null default '',
  applied_by bigint not null references rt.users(id),
  applied_at timestamptz not null default now(),
  approved_by bigint references rt.users(id),
  approved_at timestamptz,
  closed_by bigint references rt.users(id),
  closed_at timestamptz,
  note text not null default ''
);

-- 隔离持有：命中的在库批次立刻阻断新的领料/转移。已完成动作不回滚，继续追查下游。
create table if not exists rt.lot_holds (
  id bigserial primary key,
  order_id bigint not null references rt.isolation_orders(id) on delete cascade,
  event_id bigint not null references rt.quality_events(id),
  lot_id bigint not null references rt.inventory_lots(id),
  lot_key text not null,
  pile_name text not null default '',
  hit_weight_g bigint not null check (hit_weight_g >= 0),   -- 估计值（均匀占比）
  hit_lower_g bigint not null default 0,
  hit_upper_g bigint not null default 0,
  basis text not null default 'uniform_share',
  released boolean not null default false,
  released_at timestamptz,
  release_review_id bigint,
  created_at timestamptz not null default now(),
  -- 同一事件对同一批次只有一条持有：事件重试不会重复把数量加一遍。
  unique (event_id, lot_id)
);
create index if not exists lot_holds_lot_idx on rt.lot_holds(lot_id) where not released;
create index if not exists lot_holds_event_idx on rt.lot_holds(event_id);

-- 复核记录：scope_lot_ids 为空表示覆盖该事件全部持有；只覆盖部分则剩余继续阻断。
create table if not exists rt.isolation_reviews (
  id bigserial primary key,
  order_id bigint not null references rt.isolation_orders(id) on delete cascade,
  event_id bigint not null references rt.quality_events(id),
  scope_lot_ids bigint[] not null default '{}',
  scope_note text not null default '',
  coverage text not null check (coverage in ('partial','full')),
  reviewed_by bigint not null references rt.users(id),
  reviewed_at timestamptz not null default now()
);
