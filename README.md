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
- `timestamp` 可选，为带时区的 RFC3339Nano；缺省时同批全部样本共用请求开始处理的同一时刻。早于接收时刻二十四小时或晚于接收时刻五分钟的样本使整批返回 `400 {"error":{"code":"metric_timestamp_out_of_range"}}`。
- 整个批次按数组顺序原子提交；成功返回 `202` 与 `{"accepted":N}`。
- 格式或取值非法（含 `timestamp` 格式非法）返回 `400 {"error":{"code":"invalid_metrics"}}`；序列 type 或桶边界冲突返回 `409 {"error":{"code":"metric_conflict"}}`，失败批次不改变状态（既不改变当前值，也不留下历史记录）。
- 非 `application/json` 请求返回 `415`；GET/POST 以外的方法返回 `405`，`Allow: GET, POST`。

### 查询 `GET /api/v1/metrics`

`name` 精确匹配，可用重复的 `label.<key>=<value>` 筛选包含这些标签的序列：

```
GET /api/v1/metrics?name=hits&label.route=/a
```

无匹配返回 `200 {"series":[]}`。`name` 缺失或非法、选择器非法、同一标签键值冲突返回 `400 {"error":{"code":"invalid_query"}}`。结果含 `name`、`type`、`labels`；counter/gauge 另含 `value`，histogram 另含 `count`、`sum` 与按边界升序的 `buckets`（每项含 `le` 与累计 `count`，超过最大边界的观测只计入总数与总和）。序列按完整标签键值的规范化字典序排列。

### 时序查询 `GET /api/v1/metric-range`

每个租户独立保留最近二十四小时内已接受的样本（按查询时的服务器当前时间裁剪；淘汰不影响 `GET /api/v1/metrics` 的当前累计值，也不影响表达式查询、告警、通知计划与 SLO 的计算）。按固定步长窗口回看序列：

```
GET /api/v1/metric-range?name=hits&label.route=/a&start=2026-10-02T00:00:00Z&end=2026-10-03T00:00:00Z&step=3600
```

- 只接受 `name`、`label.<key>`、`start`、`end`、`step` 参数，每个参数键至多出现一次。`start`、`end` 为带时区的 RFC3339Nano 且 `start < end`；`step` 为 1 至 3600 的整数秒。时间范围为 `start <= timestamp < end`，各半开窗口从 `start` 起连续切分，窗口总数不得超过 10000。参数缺失、重复、未知，标识符、时间区间或 `step` 非法，以及窗口过多，统一返回 `400 {"error":{"code":"invalid_metric_range"}}`。
- 返回 `{"series":[...]}`，序列按完整标签规范排序，每条含 `name`、`type`、`labels` 与按窗口起点升序的 `points`；点时间戳为窗口起点，统一输出为 UTC RFC3339Nano。`start` 早于保留边界不是错误，只返回仍保留的部分。
- counter 点值为窗口内增量之和（`value`）；gauge 取窗口内时间最晚的观测，同一时刻取提交顺序靠后者（`value`）；histogram 点含窗口内 `count`、`sum` 与按该序列固定边界计算的累计 `buckets`。空窗口不生成点，无点序列不返回；没有结果时返回 `200 {"series":[]}`。
- 聚合产生非有限数时整次返回 `422 {"error":{"code":"invalid_metric_range_data"}}`，不返回部分结果。
- 仅支持 GET；其他方法返回 `405 {"error":{"code":"method_not_allowed"}}` 并设置 `Allow: GET`。该端点同样遵守 `X-SignalWatch-Tenant` 隔离。

### 表达式查询 `GET /api/v1/query`

仪表板通过一条表达式筛选并聚合当前指标。请求必须且只能携带一个 `expr` 查询参数：

```
GET /api/v1/query?expr=sum%20by%20(zone)(hits%7Broute%3D%22/a%22%7D)
```

- 选择器写作 `metric{key="value",...}`：指标名与标签键沿用标识符约束，标签值采用 JSON 字符串转义；匹配语义与 `label.<key>` 筛选一致（序列包含全部指定标签且值相等）。空选择器 `metric{}` 合法；匹配键不可重复。
- 聚合支持 `sum(selector)`、`avg(selector)`、`min(selector)`、`max(selector)`，均可写作 `sum by (label,...)(selector)` 形式按标签分组。`sum` 等关键字区分大小写；标点周围的 ASCII 空白（空格、制表、换行等）不影响含义。不接受嵌套聚合、未知函数或重复分组标签；存在尾随内容即非法。
- 查询只使用匹配的 **counter/gauge 当前值**，histogram 不参与。普通选择器为每条序列返回完整 `labels` 与 `value`；聚合按 `by` 中的标签投影分组，未写 `by` 时产生 `labels` 为空对象的单组；`avg` 按参与序列数计算。没有可用数值序列时返回空结果，不以零代替。
- 成功统一返回 `200 {"result_type":"vector","result":[{"labels":{},"value":1}]}`；`result` 按现有完整标签规范顺序排列，并基于当前租户的一致快照计算。
- 若选中的当前值或聚合结果不是有限数（如溢出为 Inf），整个请求返回 `422 {"error":{"code":"invalid_query_data"}}`，不返回部分结果。
- `expr` 缺失或重复、出现其他查询参数、语法或标识符非法、匹配键或分组键重复、存在尾随内容时，统一返回 `400 {"error":{"code":"invalid_expression"}}`。
- 仅支持 GET；其他方法返回 `405 {"error":{"code":"method_not_allowed"}}` 并设置 `Allow: GET`。该端点同样遵守 `X-SignalWatch-Tenant` 隔离，且 `invalid_tenant` 优先于表达式校验判定。

### 表达式时序查询 `GET /api/v1/query-range`

仪表板对历史区间执行与 `/api/v1/query` 相同的表达式语言。请求必须且只能携带 `expr`、`start`、`end`、`step` 四个查询参数，各出现一次：

```
GET /api/v1/query-range?expr=sum%20by%20(zone)(hits%7B%7D)&start=2026-10-03T00:00:00Z&end=2026-10-03T03:00:00Z&step=3600
```

- `expr` 沿用表达式查询的全部语法与语义：选择器 `metric{key="value",...}`、`sum`/`avg`/`min`/`max` 与可选 `by (label,...)` 分组，包括匹配键、分组键不可重复等约束。
- `start`、`end`、`step` 沿用 `/api/v1/metric-range` 的规则：带时区的 RFC3339Nano 且 `start < end`，`step` 为 1 至 3600 的整数秒；按 `[start,end)` 从 `start` 连续切分半开窗口，窗口总数不得超过 10000。`start` 早于二十四小时保留边界不是错误，只忽略已过期部分。
- 每窗口值：counter 为窗口内增量之和；gauge 取窗口内时间最晚的观测，同一时刻取提交顺序靠后者；histogram 不参与。空窗口不产生点。
- 普通选择器按完整标签分别返回时间序列；聚合表达式在每个窗口只对**当窗有值**的序列折叠，`by` 按既有标签投影分组，`avg` 的除数为该窗口实际参与的序列数；某组当窗无值时不补零。
- 成功返回 `200 {"result_type":"matrix","result":[{"labels":{},"points":[{"timestamp":"2026-10-03T00:00:00Z","value":1}]}]}`；时间戳为窗口起点，统一输出 UTC RFC3339Nano，点按时间升序，序列按完整标签规范顺序排列；没有任何点时 `result` 为空数组。计算基于当前租户的一致快照。
- 参数缺失、重复、出现未知参数，或时间格式、区间、步长、窗口数量非法时返回 `400 {"error":{"code":"invalid_query_range"}}`；`expr` 语法或标识符非法、匹配键或分组键重复时返回 `400 {"error":{"code":"invalid_expression"}}`。
- 任一窗口值或聚合结果为非有限数时整次返回 `422 {"error":{"code":"invalid_query_range_data"}}`，不返回部分结果。
- 仅支持 GET；其他方法返回 `405 {"error":{"code":"method_not_allowed"}}` 并设置 `Allow: GET`。该端点同样遵守 `X-SignalWatch-Tenant` 隔离，且 `invalid_tenant` 优先于参数与表达式校验判定。

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

## SLO 与错误预算接口

SLO 定义只保存在进程内存中，重启即清空。`id` 沿用标识符约束（`[a-zA-Z_][a-zA-Z0-9_]*`，取自路径）。定义基于进程生命周期的累计 counter，不要求被引用的指标已存在。

### 管理 SLO 定义

- `PUT /api/v1/slos/{id}`：创建或原子替换定义。新建返回 `201`，替换返回 `200`，响应体均为定义本身（含路径 `id`）。
- `GET /api/v1/slos/{id}`：查看定义，返回 `200` 与定义；不存在返回 `404 {"error":{"code":"slo_not_found"}}`。
- `DELETE /api/v1/slos/{id}`：删除定义，返回 `204`；不存在返回 `404 slo_not_found`。
- `GET /api/v1/slos`：返回 `{"slos":[...]}`，按 `id` 字典序排列；无定义时为空数组。

定义只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"objective":0.99,
 "good":{"metric":"good_events","labels":{"route":"/a"}},
 "total":{"metric":"total_events","labels":{}},
 "comment":"首页可用性"}
```

- `objective` 为 0 到 1 之间（不含端点）的有限数。
- `good`、`total` 各含 `metric` 与 `labels`；`metric` 与标签键须符合标识符约束，`labels` 必须是字符串对象（不可为 `null`）；选择器匹配包含全部指定标签的序列。
- `comment` 必须是字符串（允许空字符串）。
- 正文不是对象、字段缺失或多出、类型错误、标识符非法、标签为 `null` 或 `objective` 非法等，一律返回 `400 {"error":{"code":"invalid_slo"}}`，失败的替换不改变旧定义。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

### SLO 状态 `GET /api/v1/slo-status`

从定义与指标的**同一一致快照**评估全部定义，按 `id` 排序返回 `{"slos":[...]}`，每项含 `id`、`state`、`good_events`、`total_events`、`compliance`、`error_budget_total`、`error_budget_remaining`、`error_budget_remaining_ratio`：

- 每侧对所有匹配的 **counter** 当前值求和；gauge 与 histogram 不参与，仅匹配到它们视为无可用 counter。
- 任一侧无可用 counter，或 `total_events` 为 0：`state` 为 `no_data`；聚合结果非有限或 `good_events` 大于 `total_events`：`state` 为 `invalid_data`。两种情况下全部数值字段均为 `null`。
- 其余情况：`compliance = good_events/total_events`，`error_budget_total = total_events*(1-objective)`，`error_budget_remaining = error_budget_total-(total_events-good_events)`，`error_budget_remaining_ratio = error_budget_remaining/error_budget_total`；后两项允许为负。`compliance` 不低于 `objective` 时 `state` 为 `met`，否则为 `breached`。
- 该端点仅支持 GET（`405 method_not_allowed`，`Allow: GET`）。

## 日志接口

日志只保存在进程内存中，重启即清空。

### 写入 `POST /api/v1/logs`

仅接受 `Content-Type: application/json`（否则 `415 unsupported_media_type`）。正文包含非空 `entries` 数组，单批最多 500 条：

```json
{"entries":[{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV",
             "timestamp":"2026-01-02T03:04:05.123456789Z",
             "level":"info",
             "message":"request completed",
             "labels":{"app":"web"},
             "trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}]}
```

- `id` 为 1 至 128 个可打印 ASCII 字符；`timestamp` 为带时区的 RFC3339Nano；`level` 仅限 `debug`、`info`、`warn`、`error`；`message` 为字符串；`labels` 为字符串对象，键沿用标识符约束；`trace_id` 可选，为 32 位小写十六进制。
- 结构、未知字段或取值非法时整批返回 `400 {"error":{"code":"invalid_logs"}}`，不改变任何状态。
- 同一租户内 `id` 唯一：已有 `id` 与新记录的时间瞬间和其余内容相同视为重放，否则整批返回 `409 {"error":{"code":"log_conflict"}}`；批内重复 `id` 同样判定。成功返回 `202` 及 `{"accepted":N,"replayed":M}`；采样策略任一比例小于 1 时响应另含 `sampled_out`（见采样策略接口）。
- 每个租户按提交顺序保留最近 10000 个不同 `id`，提交后按数组顺序淘汰最早项；重放不刷新顺序，被淘汰 `id` 可再次写入。

### 检索 `GET /api/v1/logs`

首次请求可用 `start`、`end` 选择 `start <= timestamp < end` 的日志，并组合 `level`、`trace_id`、`q` 与 `label.<key>` 过滤；`q` 对 `message` 作区分大小写的子串匹配。`limit` 默认 100，范围 1 至 200：

```
GET /api/v1/logs?level=error&label.app=web&limit=50
```

- 结果按 `timestamp` 降序、同一瞬间按 `id` 字典序返回 `{"entries":[...],"next_cursor":...}`；时间统一为 UTC RFC3339Nano；空结果返回空数组与 `null` 游标。
- `next_cursor` 固定首次条件、页大小与快照上界；后续请求只能携带 `cursor`，新写入不会混入后续页，淘汰可造成缺项但不会重复。
- 非法或跨租户游标返回 `400 {"error":{"code":"invalid_cursor"}}`；其他非法参数返回 `400 {"error":{"code":"invalid_log_query"}}`。
- GET/POST 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: GET, POST`。

## 链路接口

跨度只保存在进程内存中，重启即清空。同一租户内由 `trace_id` 与 `span_id` 共同确定唯一跨度；缺失的父跨度不影响写入与查询，父跨度可以稍后到达。

### 写入 `POST /api/v1/spans`

仅接受 `Content-Type: application/json`（否则 `415 unsupported_media_type`）。正文包含非空 `spans` 数组：

```json
{"spans":[{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736",
           "span_id":"0000000000000001",
           "parent_span_id":null,
           "service":"web","name":"GET /users",
           "start_time":"2026-01-02T11:04:05+08:00",
           "end_time":"2026-01-02T11:04:06.123456789+08:00",
           "status":"ok",
           "attributes":{"http.route":"/users"}}]}
```

- 每项字段必须恰好完整：`trace_id`、`span_id`、`parent_span_id`、`service`、`name`、`start_time`、`end_time`、`status`、`attributes`。
- `trace_id` 为 32 位小写十六进制；`span_id` 为 16 位小写十六进制；`parent_span_id` 为 `null` 或同格式字符串，且不能等于自身（引用尚不存在的父跨度合法）。
- `service`、`name` 为非空字符串；`start_time`、`end_time` 为带时区的 RFC3339Nano，且结束时间不早于开始时间；`status` 仅限 `unset`、`ok`、`error`；`attributes` 为键符合标识符约束、值为字符串且均非 null 的对象（允许空对象）。
- 结构、未知字段或取值非法时整批返回 `400 {"error":{"code":"invalid_spans"}}`，不改变任何状态。
- 相同内容再次提交（包括批内重复）计为重放；同一 `(trace_id, span_id)` 提交不同内容（时间按瞬间比较，与提交时区无关）使整批返回 `409 {"error":{"code":"span_conflict"}}`。成功原子写入，返回 `202` 及 `{"accepted":N,"replayed":M}`；采样策略任一比例小于 1 时响应另含 `sampled_out`（见采样策略接口）。
- POST 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: POST`。

### 链路详情 `GET /api/v1/traces/{trace_id}`

```
GET /api/v1/traces/4bf92f3577b34da6a3ce929d0e0e4736
```

- 返回 `{"trace_id":...,"spans":[...],"logs":[...]}`。`spans` 回显跨度全部字段（`parent_span_id` 仍为 `null` 或字符串），时间统一为 UTC RFC3339Nano，按 `start_time` 升序、同一瞬间按 `span_id` 字典序排列；缺失父跨度不影响返回。
- `logs` 包含当前仍保留（未被淘汰）且 `trace_id` 相同的既有日志，保持日志原字段（含可选 `trace_id`），按 `timestamp` 升序、同一瞬间按 `id` 字典序排列。
- 没有已存跨度时返回 `404 {"error":{"code":"trace_not_found"}}`；仅有相同编号的日志不会创建链路。
- 路径编号非法返回 `400 {"error":{"code":"invalid_trace_id"}}`；该端点不接受任何查询参数，非法查询字符串返回 `400 {"error":{"code":"invalid_trace_query"}}`。
- GET 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: GET`。

### 检索 `GET /api/v1/traces`

首次请求必须各携带一次 `start`、`end`，可各携带一次 `service`、`name`、`status`、`limit`：

```
GET /api/v1/traces?start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z&service=web&status=error&limit=50
```

- `start`、`end` 为带时区的 RFC3339Nano 且 `start < end`；链路时间取该链路已存跨度的最早 `start_time`，落入 `[start, end)` 才入选。
- `service`、`name` 为非空字符串，区分大小写精确匹配；`status` 仅限 `unset`、`ok`、`error`；多个跨度过滤条件须由同一个跨度同时满足（而非分散在同一链路的不同跨度上）。
- `limit` 默认 100，范围 1 至 200。
- 返回 `{"traces":[...],"next_cursor":...}`；每个摘要只含 `trace_id`、`start_time`、`end_time`、`span_count`、`services`、`status`：起止时间分别取最早开始与最晚结束并统一为 UTC RFC3339Nano，`span_count` 为已存跨度数，`services` 去重后按字典序排列，`status` 在有 `error` 跨度时为 `error`，否则有 `ok` 时为 `ok`，否则为 `unset`。
- 摘要按 `start_time` 降序、同一瞬间按 `trace_id` 字典序排列；空结果返回空数组与 `null` 游标。
- 结果超过 `limit` 时返回不透明游标；后续请求只能携带 `cursor`，游标固定首次条件、页大小及首次请求开始时的摘要快照，此后写入的数据不会混入后续页也不会造成重复；末页游标为 `null`。
- 首次请求参数缺失、重复、未知或取值非法时返回 `400 {"error":{"code":"invalid_trace_query"}}`；游标非法、被篡改、与其他参数混用或跨租户使用时返回 `400 {"error":{"code":"invalid_trace_cursor"}}`。
- 检索只汇总当前租户的已存跨度：被采样丢弃的跨度不留任何痕迹，只有日志的 `trace_id` 不产生结果。
- GET 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: GET`。

## 采样策略接口

每个租户独立的确定性摄入采样策略只保存在进程内存中，新租户与重启后的默认值均为 `{"log_rate":1,"trace_rate":1}`。

### 读取与替换 `GET` / `PUT /api/v1/sampling-policy`

```
GET /api/v1/sampling-policy        → 200 {"log_rate":1,"trace_rate":1}
PUT /api/v1/sampling-policy        {"log_rate":0.5,"trace_rate":0.25}
```

- GET 返回当前租户策略；PUT 仅接受 `Content-Type: application/json`（否则 `415 unsupported_media_type`），正文必须恰好包含 `log_rate` 与 `trace_rate` 两个字段，均为 0 到 1 之间（含端点）的有限数；字段缺失、多余、为 null、类型错误或越界统一返回 `400 {"error":{"code":"invalid_sampling_policy"}}`，不改变现有策略。PUT 成功时原子替换整个策略并返回 `200` 与新策略。
- GET 不接受任何查询参数，携带查询字符串返回 `400 {"error":{"code":"invalid_sampling_query"}}`；GET/PUT 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: GET, PUT`。
- 该端点同样遵守 `X-SignalWatch-Tenant` 隔离，`invalid_tenant` 优先于媒体类型、正文与查询校验。

### 摄入采样语义

- 判定是确定性的：无 `trace_id` 的日志按日志 `id` 与 `log_rate` 决定去留；带 `trace_id` 的日志与全部跨度按 `trace_id` 与 `trace_rate` 决定，因此同一链路的日志与跨度去留一致，且同一租户、同一身份在相同策略下不受批次拆分、到达顺序或重放影响。
- 格式校验、批内重复识别以及与已保留记录的重放或冲突判定仍先于采样：任一非法项或冲突使整批按现有错误码失败且不改变状态；已保留身份即使后来降低比例，也继续按既有内容执行 replay 或 conflict。
- 采样只作用于尚未保留的合法项；保留项沿用现有提交顺序和原子性。丢弃项不占用 id、容量或分页快照，也不出现在日志检索和链路详情中。
- 两个比例均为 1 时，`POST /api/v1/logs` 与 `POST /api/v1/spans` 的既有行为与响应保持不变；任一比例小于 1 时，成功响应仍为 `202`，并在 `accepted`、`replayed` 之外增加 `sampled_out`，三者之和等于请求数组长度。
- 策略更新不追溯删除现有数据；此前被丢弃且从未保留的身份可在提高比例后重新提交，并按新策略判断。

## 服务发现目标接口

静态目标快照只存于内存，重启后恢复为 `generation` 为 0、`targets` 为空数组。服务仅在客户端调用抓取端点时主动访问目标地址。

### 重载 `POST /api/v1/discovery-targets/reload`

```
POST /api/v1/discovery-targets/reload
Content-Type: application/json

{"targets":[{"id":"web_1","url":"https://example.com:8443/metrics","labels":{"job":"web"},"enabled":true}]}
```

- 每个目标恰好包含 `id`、`url`、`labels`、`enabled` 四个字段：`id` 沿用标识符规则 `[a-zA-Z_][a-zA-Z0-9_]*` 且在数组内唯一；`url` 必须是主机非空的绝对 `http`/`https` URL，不允许用户信息、片段或非法端口；`labels` 是非 null 的字符串对象，键沿用指标标签键规则；`enabled` 是布尔值。
- `targets` 最多 1000 项，允许空数组（清空配置）。
- 重载先整体校验再原子替换：任一目标非法、`id` 重复、数组缺失或为 null、字段缺失或多出、正文不是单一 JSON 对象时，返回 `400 {"error":{"code":"invalid_discovery_targets"}}`，旧快照与代次均不改变；非 `application/json` 返回 `415 {"error":{"code":"unsupported_media_type"}}`。
- 成功返回 `200 {"generation":N,"targets":[...]}`；仅当 `id`、`url`、标签内容或 `enabled` 发生变化时 `generation` 才加一，目标顺序或标签键顺序不同不算变化，重复提交相同快照得到相同代次。响应目标按 `id` 字典序排列。
- POST 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: POST`。

### 查询 `GET /api/v1/discovery-targets`

- 从一致快照返回当前租户的完整配置 `{"generation":N,"targets":[...]}`，目标按 `id` 字典序排列；合法新租户返回 `generation` 为 0、空 `targets` 数组。
- 不接受任何查询参数，携带查询参数返回 `400 {"error":{"code":"invalid_discovery_query"}}`。
- GET 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: GET`。

### 主动抓取 `POST /api/v1/discovery-targets/scrape`

```
POST /api/v1/discovery-targets/scrape
Content-Type: application/json

{"target_ids":["web_1"]}
```

- 正文必须是恰好包含 `target_ids` 数组的单一 JSON 对象：每个编号须符合标识符规则且不重复；非空时抓取指定目标，空数组抓取全部 `enabled` 目标。结构、字段或编号非法返回 `400 {"error":{"code":"invalid_discovery_scrape"}}`；编号不存在返回 `404 {"error":{"code":"discovery_target_not_found"}}`（先于 disabled 判定）；选择了 disabled 目标返回 `409 {"error":{"code":"discovery_target_disabled"}}`。非 `application/json` 返回 `415 unsupported_media_type`。
- 请求开始时固定 `generation` 与目标快照，期间的重载不影响本次抓取。
- 服务以 GET 访问目标 URL，五秒超时且不跟随重定向，只接受 200 状态的 Prometheus 文本。样本为 `metric{label="value"} number` 或无标签形式，类型由最近的 `TYPE` 声明确定且必须是 `counter` 或 `gauge`；未知类型、重复标签、非法标识符或非有限数均使该目标记为 `invalid_exposition`，不写入任何指标。
- 静态标签与样本标签合并：同名异值、或任一来源使用保留标签 `target_id` 时记为 `label_conflict`；否则为每条序列补入 `target_id=<目标编号>`。
- `gauge` 覆盖当前值；`counter` 视为累计值：首次写入全值，之后未下降时写入差值，下降时按重置写入新值。计数基线仅随整批提交成功更新；与既有序列类型冲突记为 `metric_conflict`，该目标的指标与基线均不变。
- 目标访问失败记 `target_unreachable`，非 200 响应记 `target_http_error`；任何单个目标的失败都不回滚其他目标。
- 成功返回 `200 {"generation":N,"results":[...]}`，`results` 按编号排序，每项含 `id`、`status`（`ok` 或 `error`）与 `accepted`；失败项 `accepted` 为零并另含对应 `code`。目标按编号顺序提交；无 enabled 目标时 `results` 为空数组。
- POST 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}`，`Allow: POST`。该端点同样遵守 `X-SignalWatch-Tenant` 隔离，`invalid_tenant` 优先于其他校验。

## 诊断导出接口

`POST /api/v1/diagnostic-export` 只读生成当前租户的 JSON 诊断包，不改变任何状态。

```
POST /api/v1/diagnostic-export
Content-Type: application/json

{"start":"2026-10-04T00:00:00Z","end":"2026-10-05T00:00:00Z",
 "sections":["metrics","alerts","logs","traces","configuration"]}
```

- 仅接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须恰好包含 `start`、`end`、`sections`：`start`、`end` 为带时区的 RFC3339Nano 且 `start < end`，跨度最多二十四小时；`sections` 为从 `metrics`、`alerts`、`logs`、`traces`、`configuration` 中选择的非空无重复数组。正文、时间、区间或节名非法统一返回 `400 {"error":{"code":"invalid_diagnostic_export"}}`。
- 成功返回 `200`，主体含 `schema_version`（固定为 1）、`tenant`、`generated_at`、`range`（`start`/`end`）与 `sections`；所有时间统一为 UTC RFC3339Nano，`sections` 只出现所选节。各节取自请求开始时当前租户的同一一致快照，`generated_at` 即该时刻，并发变更不会混入同一导出。
- `metrics`：`current` 沿用指标查询结构返回全部序列当前值；`samples` 返回保留期内处于 `[start,end)` 的原始观测，含 `name`、`type`、`labels`、`value`、`timestamp`，histogram 另含 `buckets`（序列固定边界），counter 保持写入增量；样本按 `timestamp`、序列规范键、提交顺序排列。
- `alerts`：含现有告警状态（`alerts`）、通知计划（`notification_plan`，含 `deliveries` 与 `unrouted_alert_ids`）与 SLO 状态（`slo_status.slos`），语义与对应端点一致。
- `logs`：`entries` 返回区间内仍保留的完整日志，保持日志检索的字段与排序（`timestamp` 降序、同一瞬间 `id` 升序）。
- `traces`：`spans` 返回 `start_time` 处于区间内的完整跨度，按 `start_time`、`trace_id`、`span_id` 升序。
- `configuration`：含告警规则（`alert_rules`）、静默（`silences`）、抑制规则（`inhibit_rules`）、通知路由（`notification_routes`）、SLO 定义（`slos`）、采样策略（`sampling_policy`）与发现目标（`discovery_targets`），各字段沿用对应读取端点的线格式。
- 编码后结果超过 10 MiB 返回 `413 {"error":{"code":"diagnostic_export_too_large"}}`，不返回部分内容；任一数值为非有限 JSON 数时整次返回 `422 {"error":{"code":"invalid_diagnostic_export_data"}}`。
- POST 以外的方法返回 `405 {"error":{"code":"method_not_allowed"}}` 并设置 `Allow: POST`。该端点同样遵守 `X-SignalWatch-Tenant` 隔离，`invalid_tenant` 优先于媒体类型与正文校验。

## 租户隔离

所有已注册的 `/api/v1` 端点（指标、时序查询、表达式查询、表达式时序查询、告警规则、静默、抑制规则、通知路由、SLO 及其状态/计划计算、日志写入与检索、跨度写入与链路详情、服务发现目标的重载、查询与抓取）均按租户隔离。客户端通过请求头 `X-SignalWatch-Tenant` 选择租户；不携带该头时固定进入 `default` 租户，因此不带租户头的既有客户端行为不变。

- 显式租户值必须**精确**匹配 `[a-zA-Z_][a-zA-Z0-9_-]{0,63}`，不做大小写折叠，也不修剪首尾空白。
- 请求头缺失 → `default`；出现多个头值、空值或不符合格式的值 → 在解析媒体类型、正文与资源编号之前返回 `400 {"error":{"code":"invalid_tenant"}}`，且不改变任何租户的状态。
- 每个租户拥有独立的指标序列、日志、跨度、告警规则、静默、抑制规则、通知路由、SLO 与服务发现目标快照：同名指标/标签集、同编号跨度（`trace_id`+`span_id`）与同编号资源可在不同租户并存，读写删及规则选择器、静默/抑制引用、路由匹配、SLO 聚合、链路详情的日志关联、目标快照的重载与查询均不跨租户解析；批量提交的原子性与告警、路由计划、SLO 计算的一致快照范围均限定在当前租户。
- 不同租户使用各自独立的锁与存储，并发访问不同租户既不会串读，也不会互相阻塞；全部状态仍只存于进程内存，重启统一清空。
- 首次访问尚无数据的合法租户时，集合与计算端点按基线空状态响应（空数组），单项查询/删除沿用对应资源既有的 `not_found` 错误。
- `GET /healthz` 不参与租户隔离并忽略该头；未知路径保持基线 404 行为。

## 验证

```bash
go test ./...
```

