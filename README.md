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

从同一一致快照计算全部规则与静默，按 `id` 排序返回 `{"alerts":[...]}`，每项含 `id`、`state`、`value`、`silenced`、`silence_ids`：

- 每侧对所有匹配的 counter/gauge 当前值求和；histogram 不参与，仅匹配到 histogram 视为无可用序列。
- 任一侧无可用序列，或 ratio 分母之和为零：`state` 为 `no_data`，`value` 为 `null`。
- 其余情况 threshold 的 `value` 为和值，ratio 的 `value` 为分子之和除以分母之和；比较成立为 `firing`，否则为 `inactive`。
- 仅当 `state` 为 `firing`，且当前时刻满足 `starts_at <= now < ends_at`、静默包含该规则编号时，`silenced` 为 `true`；`silence_ids` 收集所有命中的活动静默编号并按字典序排列，其他情况为 `false` 与空数组。

## 告警静默接口

静默仅保存在进程内存中，重启即清空。静默 `id` 与正文 `rule_ids` 中的编号均沿用规则编号约束（`[a-zA-Z_][a-zA-Z0-9_]*`，静默 `id` 取自路径）。创建静默时不要求引用的规则已经存在，可以先安排维护窗口。已过期静默继续保留，直到显式删除。

### 管理静默

- `PUT /api/v1/silences/{id}`：创建或原子替换静默。新建返回 `201`，替换返回 `200`，响应体均为静默本身。
- `GET /api/v1/silences/{id}`：查看静默，返回 `200` 与静默；不存在返回 `404 {"error":{"code":"silence_not_found"}}`。
- `DELETE /api/v1/silences/{id}`：删除静默，返回 `204`；不存在返回 `404 silence_not_found`。
- `GET /api/v1/silences`：返回 `{"silences":[...]}`，按 `id` 字典序排列；无静默时为空数组。

静默只接受 `application/json`（否则 `415 unsupported_media_type`）。正文必须是单个对象，字段必须恰好完整：

```json
{"rule_ids":["high_hits"],"starts_at":"2026-10-03T10:00:00+08:00","ends_at":"2026-10-03T12:00:00+08:00","comment":"维护窗口"}
```

- `rule_ids` 为非空字符串数组，编号须符合标识符约束且不重复；规则无须预先存在。
- `starts_at`、`ends_at` 为带时区的 RFC3339 时间字符串，且 `starts_at` 必须严格早于 `ends_at`。
- `comment` 必须是字符串（允许空字符串）。
- JSON 结构、字段、编号、时间或时间区间不合法，或正文多出字段，一律返回 `400 {"error":{"code":"invalid_silence"}}`，失败的替换不改变旧静默。
- 每项响应除 `id` 与原始字段外，另含由当前时间确定的 `status`：`upcoming`（未开始）、`active`（`starts_at <= now < ends_at`）或 `expired`（已结束）。
- 集合端点仅支持 GET（`405`，`Allow: GET`）；单项端点支持 GET、PUT、DELETE（`405`，`Allow: GET, PUT, DELETE`）。

## 验证

```bash
go test ./...
```

