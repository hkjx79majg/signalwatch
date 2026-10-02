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

## 告警规则

规则同样只保存在进程内存中，重启即清空。规则 id 沿用指标名约束 `[a-zA-Z_][a-zA-Z0-9_]*`。

### 写入 `PUT /api/v1/alert-rules/{id}`

仅接受 `Content-Type: application/json`。正文为单个规则对象，`kind` 为 `threshold` 或 `ratio`，`operator` 只接受 `gt`、`gte`、`lt`、`lte`，`threshold` 必须是有限数：

```json
{"kind":"threshold","metric":"hits","labels":{"route":"/a"},"operator":"gt","threshold":10}
{"kind":"ratio","numerator":{"metric":"err","labels":{}},"denominator":{"metric":"tot","labels":{}},"operator":"gte","threshold":0.5}
```

- `threshold` 规则用 `metric` 与 `labels` 选择序列；`ratio` 规则用 `numerator`、`denominator` 各提供一组 `metric` 与 `labels`。选择器匹配包含全部指定标签的序列。
- 字段必须与 `kind` 相符：缺少或多出字段、类型错误、标识符或选择器非法、阈值非有限数、运算符不支持均返回 `400 {"error":{"code":"invalid_rule"}}`，原规则不变。
- 新建返回 `201`，原子替换返回 `200`，响应体均为规则本身（含 `id`）。非 `application/json` 返回 `415 {"error":{"code":"unsupported_media_type"}}`。

### 读取与删除

- `GET /api/v1/alert-rules` 返回 `{"rules":[...]}`，按 id 字典序排列。
- `GET /api/v1/alert-rules/{id}` 返回 `200` 及规则；`DELETE /api/v1/alert-rules/{id}` 返回 `204`。不存在的 id 返回 `404 {"error":{"code":"rule_not_found"}}`。
- 三个端点不支持的方法返回 `405 {"error":{"code":"method_not_allowed"}}` 及相应 `Allow` 头。

### 求值 `GET /api/v1/alerts`

从同一一致快照计算全部规则，按 id 排序返回 `{"alerts":[...]}`，每项含 `id`、`state`、`value`：

- 每侧只对匹配的 counter 与 gauge 当前值求和，histogram 不参与；仅匹配 histogram 视为无可用序列。
- 比较成立为 `firing`，否则为 `inactive`；任一侧无可用序列或比例分母和为零时为 `no_data`，此时 `value` 为 `null`。
- 其余情况 `value` 为阈值规则的和值或比例规则的商。

## 验证

```bash
go test ./...
```

