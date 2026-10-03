# SignalWatch

这是一个面向可观测性的可观测性与指标告警平台。长期目标是提供指标采集与标签模型、时序存储与保留策略、告警规则与分组抑制、通知路由、链路追踪、日志检索和 SLO 错误预算，把可观测性沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/signalwatch
```

服务默认监听 `127.0.0.1:8080`。可通过 `SIGNALWATCH_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 指标接口

状态仅保存在进程内存中，重启即清空。

### 写入 `POST /api/v1/metrics`

仅接受 `Content-Type: application/json`。正文包含非空 `samples` 数组：

```json
{"samples":[{"name":"hits","type":"counter","labels":{"route":"/a"},"value":3}]}
```

- `name` 与标签键匹配 `[a-zA-Z_][a-zA-Z0-9_]*`；`labels` 为字符串到字符串的对象，指标名与完整标签集（与键顺序无关）唯一确定一条序列。
- `type` 为 `counter`（非负增量累加）、`gauge`（覆盖旧值）或 `histogram`（记录观测）。
- `value` 必须是有限数；`counter` 不接受负数。
- `histogram` 额外携带严格递增的有限数 `buckets`，边界在该序列首次写入时确定，后续写入必须完全一致。
- 整个批次按数组顺序原子提交；成功返回 `202` 与 `{"accepted":N}`。
- 格式或取值非法返回 `400 {"error":{"code":"invalid_metrics"}}`；序列 type 或桶边界冲突返回 `409 {"error":{"code":"metric_conflict"}}`，失败批次不改变状态。
- 非 `application/json` 请求返回 `415`；GET/POST 以外的方法返回 `405`，`Allow: GET, POST`。

### 查询 `GET /api/v1/metrics`

`name` 精确匹配，可用重复的 `label.<key>=<value>` 筛选包含这些标签的序列：

```
GET /api/v1/metrics?name=hits&label.route=/a
```

无匹配返回 `200 {"series":[]}`。`name` 缺失或非法、选择器非法、同一标签键值冲突返回 `400 {"error":{"code":"invalid_query"}}`。结果含 `name`、`type`、`labels`；counter/gauge 另含 `value`，histogram 另含 `count`、`sum` 与按边界升序的 `buckets`（每项含 `le` 与累计 `count`，超过最大边界的观测只计入总数与总和）。序列按完整标签键值的规范化字典序排列。

## 告警规则接口

规则仅保存在进程内存中，重启即清空。规则 `id` 沿用指标名约束（`[a-zA-Z_][a-zA-Z0-9_]*`，取自路径）。

### 管理规则

- `PUT /api/v1/alert-rules/{id}`：创建或原子替换规则。新建返回 `201`，替换返回 `200`，响应体均为规则本身。
- `GET /api/v1/alert-rules/{id}`：查看规则，返回 `200` 与规则；不存在返回 `404 {"error":{"code":"rule_not_found"}}`。
- `DELETE /api/v1/alert-rules/{id}`：删除规则，返回 `204`；不存在返回 `404 rule_not_found`。
- `GET /api/v1/alert-rules`：返回 `{"rules":[...]}`，按 `id` 字典序排列；无规则时为空数组。

规则只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"kind":"threshold","operator":"gt","threshold":100,"metric":"hits","labels":{"route":"/a"}}
```

```json
{"kind":"ratio","operator":"lt","threshold":0.05,
 "numerator":{"metric":"errors","labels":{}},
 "denominator":{"metric":"requests","labels":{}}}
```

- `kind` 为 `threshold` 或 `ratio`；threshold 规则含顶层 `metric`/`labels`，ratio 规则含 `numerator`/`denominator`（各自含 `metric`/`labels`），条件字段与 `kind` 不符（含任一侧多余字段）均为非法。
- `operator` 只接受 `gt`、`gte`、`lt`、`lte`；`threshold` 必须是有限数。
- `metric` 与标签键须符合标识符约束，`labels` 必须是字符串对象；选择器匹配包含全部指定标签的序列。
- 正文不是对象、字段缺失或多出、类型错误、标识符或选择器非法、阈值非有限数等，一律返回 `400 {"error":{"code":"invalid_rule"}}`，且已有规则不被改变。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

### 告警状态 `GET /api/v1/alerts`

从同一一致快照计算全部规则、静默与抑制，按 `id` 排序返回 `{"alerts":[...]}`，每项含 `id`、`state`、`value`、`silenced`、`silence_ids`、`inhibited`、`inhibition_ids`：

- 每侧对所有匹配的 counter/gauge 当前值求和；histogram 不参与，仅匹配到 histogram 视为无可用序列。
- 任一侧无可用序列，或 ratio 分母之和为零：`state` 为 `no_data`，`value` 为 `null`。
- 其余情况 threshold 的 `value` 为和值，ratio 的 `value` 为分子之和除以分母之和；比较成立为 `firing`，否则为 `inactive`。
- 仅当 `state` 为 `firing`，且某活动静默满足 `starts_at <= 当前时间 < ends_at` 并包含该规则编号时，`silenced` 为 `true`；`silence_ids` 收集所有命中的活动静默编号并按字典序排列。其他情况 `silenced` 为 `false`、`silence_ids` 为空数组。
- 静默结果算完后再计算抑制：仅当告警原始 `state` 为 `firing`，且某条抑制规则包含该告警编号、同时至少一个**现存源告警**原始 `state` 也为 `firing` 时命中。源告警即使自身被静默或被其他规则抑制，仍可作为触发源；规则可引用尚不存在的告警。
- `inhibition_ids` 收集全部命中的抑制规则编号并按字典序排列；命中时 `inhibited` 为 `true`，未命中或原始状态为 `inactive`/`no_data` 时为 `false`、`inhibition_ids` 为空数组。抑制不改变告警的 `state`、`value`、`silenced`、`silence_ids` 与排序。

## 告警静默接口

静默与指标、规则一样只保存在进程内存中，重启即清空。静默 `id` 沿用规则编号约束（`[a-zA-Z_][a-zA-Z0-9_]*`，取自路径）。创建静默时不要求被引用的规则已存在。

### 管理静默

- `PUT /api/v1/silences/{id}`：创建或原子替换静默。新建返回 `201`，替换返回 `200`，响应体均为静默本身。
- `GET /api/v1/silences/{id}`：查看静默，返回 `200` 与静默；不存在返回 `404 {"error":{"code":"silence_not_found"}}`。
- `DELETE /api/v1/silences/{id}`：删除静默，返回 `204`；不存在返回 `404 silence_not_found`。已过期静默不会自动删除，需显式删除。
- `GET /api/v1/silences`：返回 `{"silences":[...]}`，按 `id` 字典序排列；无静默时为空数组。

静默只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"rule_ids":["high_hits","ratio_fire"],
 "starts_at":"2026-01-01T00:00:00Z",
 "ends_at":"2026-01-01T02:00:00+08:00",
 "comment":"数据库维护窗口"}
```

- `rule_ids` 为非空字符串数组，各项符合标识符约束且不重复。
- `starts_at`、`ends_at` 为带时区的 RFC3339 字符串，且 `starts_at` 必须严格早于 `ends_at`。
- `comment` 必须是字符串（允许空字符串）。
- 正文不是对象、字段缺失或多出、类型错误、编号非法或重复、时间格式非法、时间区间非法等，一律返回 `400 {"error":{"code":"invalid_silence"}}`，失败的替换不改变旧静默。
- 每项响应在原始字段外另含由当前时间确定的 `state`：`upcoming`（当前时间早于 `starts_at`）、`active`（`starts_at <= 当前时间 < ends_at`）或 `expired`（当前时间不早于 `ends_at`）。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

## 告警抑制规则接口

抑制规则只保存在进程内存中，重启即清空。规则 `id` 沿用告警规则编号约束（`[a-zA-Z_][a-zA-Z0-9_]*`，取自路径）。规则可引用尚不存在的告警规则编号。

### 管理抑制规则

- `PUT /api/v1/inhibit-rules/{id}`：创建或原子替换抑制规则。新建返回 `201`，替换返回 `200`，响应体均为规则本身（含路径 `id`）。
- `GET /api/v1/inhibit-rules/{id}`：查看规则，返回 `200` 与规则；不存在返回 `404 {"error":{"code":"inhibit_rule_not_found"}}`。
- `DELETE /api/v1/inhibit-rules/{id}`：删除规则，返回 `204`；不存在返回 `404 inhibit_rule_not_found`。
- `GET /api/v1/inhibit-rules`：返回 `{"rules":[...]}`，按 `id` 字典序排列；无规则时为空数组。

规则只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"source_rule_ids":["node_down"],
 "target_rule_ids":["high_cpu","high_disk"],
 "comment":"节点宕机时抑制其派生告警"}
```

- `source_rule_ids`、`target_rule_ids` 均为非空字符串数组，各项符合标识符约束，同一数组内不得重复，源集合与目标集合不得相交。
- `comment` 必须是字符串（允许空字符串）。
- 正文不是对象、多段 JSON、字段缺失或多出、类型错误、编号非法、数组为空、编号重复或源目交叉等，一律返回 `400 {"error":{"code":"invalid_inhibit_rule"}}`，失败的替换不改变旧规则。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

## 通知路由接口

通知路由只保存在进程内存中，重启即清空。路由 `id` 与 `receiver` 均沿用标识符约束（`[a-zA-Z_][a-zA-Z0-9_]*`，`id` 取自路径）。创建路由时不要求被引用的告警规则已存在。

### 管理路由

- `PUT /api/v1/notification-routes/{id}`：创建或原子替换路由。新建返回 `201`，替换返回 `200`，响应体均为路由本身（含路径 `id`）。
- `GET /api/v1/notification-routes/{id}`：查看路由，返回 `200` 与路由；不存在返回 `404 {"error":{"code":"notification_route_not_found"}}`。
- `DELETE /api/v1/notification-routes/{id}`：删除路由，返回 `204`；不存在返回 `404 notification_route_not_found`。
- `GET /api/v1/notification-routes`：返回 `{"routes":[...]}`，按 `id` 字典序排列；无路由时为空数组。

路由只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"rule_ids":["high_hits","ratio_fire"],
 "receiver":"oncall_team",
 "priority":100,
 "comment":"高优路由"}
```

- `rule_ids` 为非空字符串数组，各项符合标识符约束且不重复；可引用尚不存在的告警规则编号。
- `receiver` 必须符合标识符约束。
- `priority` 为 `0` 至 `1000` 的整数（含端点）。
- `comment` 为可空字符串：必须是字符串或 `null`（GET 原样回显，`null` 回显为 `null`），但字段不能缺失。
- 正文不是对象、多段 JSON、字段缺失或多出、类型错误、编号非法、数组为空、编号重复、`receiver` 非法或优先级越界/非整数等，一律返回 `400 {"error":{"code":"invalid_notification_route"}}`，失败的替换不改变旧路由。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

### 路由计划 `GET /api/v1/notification-plan`

使用指标、告警规则、静默、抑制与通知路由的**同一一致快照**计算，返回：

```json
{"deliveries":[{"alert_id":"high_hits","receiver":"oncall_team","route_id":"nr1"}],
 "unrouted_alert_ids":["other_alert"]}
```

- 仅 `state` 为 `firing` 且 `silenced`、`inhibited` 均为 `false` 的告警参与；`inactive`、`no_data`、被静默或被抑制的告警既不投递也不计入未路由。
- 每个参与告警从**包含其规则编号**的路由中选择 `priority` 最小者；优先级相同取 `id` 字典序最小者。
- 匹配项写入 `deliveries`，每项恰好含 `alert_id`、`receiver`、`route_id`；没有任何路由包含其规则编号的参与告警写入 `unrouted_alert_ids`。
- `deliveries` 按 `alert_id` 字典序排列，`unrouted_alert_ids` 按字典序排列；无结果时二者均为空数组。
- 该端点仅支持 GET（`405 method_not_allowed`，`Allow: GET`）。

## 验证

```bash
go test ./...
```

