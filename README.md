# SignalWatch

这是一个面向可观测性的可观测性与指标告警平台。长期目标是提供指标采集与标签模型、时序存储与保留策略、告警规则与分组抑制、通知路由、链路追踪、日志检索和 SLO 错误预算，把可观测性沉淀为可复用平台。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/signalwatch
```

服务默认监听 `127.0.0.1:8080`。可通过 `SIGNALWATCH_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 指标采集与查询

指标状态仅保存在内存中，重启即清空。序列由指标名与完整（无序）标签集唯一确定。

### `POST /api/v1/metrics`

仅接收 `application/json`。正文须含非空 `samples` 数组；每个样本含 `name`、`type`、`labels`、`value`：

- `type` 为 `counter`、`gauge` 或 `histogram`；`name` 与标签键须匹配 `[a-zA-Z_][a-zA-Z0-9_]*`，`labels` 为字符串对象。
- `value` 须为有限数：`counter` 接受非负增量并累加，`gauge` 覆盖旧值，`histogram` 记录观测。
- `histogram` 样本另带严格递增的有限数 `buckets`；首次写入确定边界，后续写入须完全一致。聚合含 `count`、`sum` 与各边界累计计数，超过最大边界的观测仍计入总数与总和。

批次按数组顺序原子提交，失败批次不改变任何状态。成功返回 `202` 与 `{"accepted":N}`；格式或取值无效返回 `400` 与 `{"error":{"code":"invalid_metrics"}}`；序列类型或边界冲突返回 `409` 与 `{"error":{"code":"metric_conflict"}}`；非 `application/json` 返回 `415`；其他方法返回 `405`（`Allow: GET, POST`）。

### `GET /api/v1/metrics`

- `name` 为必填精确匹配；可重复使用 `label.<key>=<value>` 筛选包含这些标签的序列。
- 无匹配返回 `200` 与 `{"series":[]}`；`name` 缺失或非法、选择器非法、同一标签键值冲突返回 `400` 与 `{"error":{"code":"invalid_query"}}`。
- 每条结果含 `name`、`type`、`labels`；`counter`/`gauge` 另含 `value`，`histogram` 另含 `count`、`sum` 与按边界升序的 `buckets`（含 `le` 与累计 `count`）。序列按完整标签键值的规范化字典序排列。

## 验证

```bash
go test ./...
```

时序存储与告警规则等能力仍刻意留白，以便后续任务从已冻结事实出发独立设计并验证这些能力。
