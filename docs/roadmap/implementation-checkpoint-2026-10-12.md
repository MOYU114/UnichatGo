# Implementation checkpoint: 2026-10-12

## Honest status

用户已明确授权从规划进入本地隔离实施。本轮不是仅提交方案，已经新增代码、迁移、前端和真实服务测试。但 **M1-M6 尚未全部完成，不具备生产灰度条件**。M7 没有虚报优化。保持异步配置默认关闭；无生产操作、真实用户数据、付费模型或远端推送。

本轮在长执行/上下文检查点收束，**不是完成验收，也不是等待用户重新批准继续实施**。续接必须先读取本检查点、当前 Git 状态与 review-001~005，保留现有未提交成果，然后从下面列出的 M3.5 功能等价缺口开始；不得重新初始化或用较早版本覆盖当前工作。

起始 Git commit `a9134d14f408fc712b7c23a504091cad2d5aa2b5`；起始状态只有 `?? docs/`，是用户已有成果，已读取并保留。未发现适用 AGENTS.md。原始 issue/三轮讨论/ADR-001~003 均读取，未将已被替代的 MySQL 常态领取队列重新作为默认方案。新建英文路径文档与代码，没有回滚、覆盖用户已有实现或提交 Git commit。

## Delivered and validated

| 范围 | 实际产物及证据 | 不能推定的结论 |
| --- | --- | --- |
| M1 | ADR-004 字段/索引/状态/事务/故障边界/API/迁移设计；review-001 独立审阅及修正 | 设计不等于全部功能验收 |
| M2 | `backend/internal/execution/` additive version migration、compact evidence、SQL-first acceptance、binary idempotency、sequence/lease/fence、atomic completion/history | SQLite 不是事务证据；旧生产数据未迁移 |
| M3 主链路 | Outbox generation CAS、Streams 阻塞拉取/PEL reclaim、有限预取、SQL durable terminal/handoff 后 ACK、毒消息隔离 | 辅助/Agent 多轮调用尚未等价迁移 |
| M4 主链路 | HTTP 202、独立 worker role/lifecycle、SQL 状态历史、unknown/uncertain、显式审计 abandon、结果保存有限重试、优雅下线 | 没有真实浏览器交互验证；全 DB 服务停机未演练 |
| M5 基础 | SQL global concurrency/RPM/TPM reservation、配置/密钥绑定、共享 MySQL 对象、pin/delete 竞争保护、旧文件 dry-run/copy 工具、readiness/聚合指标 | 完整附件摘要/工具/历史复用未完成；未配置外部告警/HA |
| M6 部分 | 实际两个 API + 两个 Worker 子进程、独立本地目录、mock HTTP provider、API/Worker SIGKILL、Redis stream 丢失后的恢复 | 同一主机独立目录不是两台物理主机；没有物理存储灾难/容量/SLO/生产灰度 |

新链路从 SQL 读取有效历史，不使用旧本机/Redis 历史缓存；当前输入仅一次，未来 queued 和失败轮次不进入上下文。已完成凭证随会话保留。冻结快照用现有 AES-GCM 密钥加密，队列/历史不含 Provider Key。配额/受理/密钥/Provider 路由配置指纹拒绝跨实例漂移。正常 JSON Provider 结果必须有显式完成标记；不完整响应不保存为成功；没有 SDK 自动重试。

## Independent review and fixes

重要变更均由未负责该实现的 agent 审阅。主 agent 阅读并核实了发现、实现与测试；未仅转发结论。范围包含新建未跟踪文件、配置、前端、迁移和文档。

- `review-001.md`: M1 设计的配额 lease、Outbox CAS、删除证据、legacy 历史 cutoff、幂等 hash 分层等问题。
- `review-002.md`: repository/Provider/前端审查；session 删除绕过受理 RPM、migration advisory-lock 长度、未定义多次调用接口；前端持久 journal、Web Locks、错误码、删除失败误登出等。
- `review-003.md`: API/runtime/auth/配置/进程测试；原子历史、旧 Redis auth 缓存、503 错误归因、附件 transient error、binary client ID、迁移路径/FIFO/输出失败等。
- `review-004.md`: 前端实施后独立复审、repository/provider 全文件审查、attachment race/API/process/fault-connector 测试独立审阅、测试脚本脱敏和 failure cleanup。
- 追加修复：短暂续租错误不立即取消最终保存；provider context 与保存 context 分离，SQL lease/fence 仍为最终提交依据；真实 MySQL 注入测试证明只保存原回复且仅一次 Provider 调用。

审查中已发现的 P0/P1/P2 正确性、安全和一致性问题均逐项处理；功能等价和验收覆盖欠缺保持为阻塞项，不能以“review 没新发现”将其豁免。

## Executed environment and commands

隔离测试容器（本轮创建，不挂载旧数据）：`unichatgo-async-mysql-test` MySQL 8.4，绑定 `127.0.0.1:13316`；`unichatgo-async-redis-test` Redis 7.4，绑定 `127.0.0.1:16386`。测试库 `unichatgo_async_test` 与测试自建随机 schema；API/process tests 自建 schema 后仅清理自己创建的库；Redis logical DB 12/14/15 仅用于测试。所有 Provider 调用使用本机假 HTTP 服务/可控 channel。

实际执行过：

```sh
cd backend
go test ./...
UNICHATGO_TEST_MYSQL_DSN='<isolated-test-dsn>' \
UNICHATGO_TEST_REDIS_ADDR='127.0.0.1:16386' \
go test ./internal/execution -count=1 -v
GIN_MODE=release UNICHATGO_TEST_MYSQL_DSN='<isolated-test-dsn>' \
UNICHATGO_TEST_REDIS_ADDR='127.0.0.1:16386' \
go test -race ./internal/api -run TestAsyncMySQL -count=1
UNICHATGO_TEST_PROCESS=1 UNICHATGO_TEST_MYSQL_DSN='<isolated-test-dsn>' \
UNICHATGO_TEST_REDIS_ADDR='127.0.0.1:16386' \
go test ./internal/api -run TestAsyncProcess -count=1 -v
UNICHATGO_TEST_MYSQL_DSN='<isolated-test-dsn>' \
go test -race ./internal/execution -run TestMySQLRuntimeSavesOriginalAfterTransientSQLFailure -count=1 -v
cd ..
npm --prefix frontend test
npm --prefix frontend run build
```

真实 SQL 验证覆盖跨对象 barrier 并发 key、完成后/重启后重发、payload 冲突/越权、唯一消息、事务 trigger 回滚、顺序/历史 cutoff、outbox CAS、已提交未 ACK、过期 pre/post-call、旧 owner 写回拒绝、uncertain 审计/阻塞、全局配额保留、共享对象/pin/清理/delete 竞争、配置不一致拒绝、增量迁移 rerun/未来版本拒绝。API 测试覆盖鉴权 503 与明文泄漏否定断言。进程测试断言 A 上传、B 受理、Worker 读取同一字节；API kill 后保存、Redis 丢 stream 补投、模型进入后 Worker kill 转 uncertain 而不重复调用。

续租/结果保存故障用真实 MySQL driver wrapper 注入非 `ErrBadConn` 临时语句错误，由 channel 观察恢复而非 sleep 推测。它证明原回复保留，不等同完整 MySQL 停机。所有用例的计时超时仅用于失败截止；关键业务顺序由 barrier/channel 或 SQL 明确调整 lease 断言。

单项实际结果：Provider/image 回归通过；初始 repository 7 项 1.19s，后续加入 policy/attachments/API 回归通过；API 5 项 race 4.656s；OS 子进程场景初次 18.63s，后续 14.458s；save recovery 两子项 0.21s/2.20s；前端 38 项及构建通过。时间是该次运行记录，不是容量或 SLO。

### Full-run history (including failures)

1. 初始 `go test ./...` 在各实现文件并行生成期间编译失败；之后整合后重跑通过。Go 缓存/本地监听受沙箱限制的命令均正常申请工具审批，没有绕过。
2. 全脚本 `run.OrX0NV`：所选异步后端 race/前端测试/build 通过，但 3 项旧 Redis 测试因环境变量未设置而跳过，**不计为全通过**。脚本随后强制显式 TEST_REDIS_ADDR/DB15，任何 skip 视为未完成。
3. 全脚本 `run.jXi7gH`：异步 API/process/execution 通过，但旧鉴权测试错误期待 SQL token 已删除后仍凭 Redis 授权；旧 pubsub 测试暴露 Subscribe 未确认就 publish 的竞争，整体 **失败**，未执行该轮前端步骤。这两项纳入修复与复审，不能隐藏失败。
4. 修复并独立复审后，最终 `run.H2A3tS` 于新加坡时间 2026-10-12 13:29:01 开始：`go test -p 1 -race -count=1 -v ./...` **65 个顶层测试、含子测试 108 个 PASS，0 失败、0 skip**；前端 **38/38** 测试通过；Vite build 通过（1.89s）。API 包 20.096s、auth 1.962s、execution 6.212s、assistant 2.501s、worker 2.792s；OS process 场景 14.95s、save recovery 2.35s。主 agent 实际读取了报告并核对尾部结果。保留 Vite bundle 大小与 macOS linker 非阻塞 warning，不宣称生产容量。
5. 随后 `go vet ./...` 发现测试复制含 `atomic.Int64/noCopy` 的 Runtime；已改成显式构造新 Runtime并 gofmt，frontend agent 独立复审通过。`go vet ./...` 再执行无诊断；真实 MySQL/Redis `go test -race ./internal/execution -run TestRuntimeRealServices -count=1` 通过（2.223s）。这是测试构造修正，不改变执行协议。
6. 用户报告回归后的独立修复轮次：`run.sjEIBU` 于新加坡时间 2026-10-12 15:01:28 开始，完整脚本通过。后端 `go test -p 1 -race -count=1 -v ./...` **76 个顶层测试、含子测试 137 个 PASS，0 失败、0 skip**；前端 **53/53**、0 skip，生产构建通过（1.97s）。API 27.349s、auth 2.015s、config 1.394s、execution 7.274s、assistant 2.534s、worker 2.856s；两 API/两 Worker 进程测试 21.39s、默认图片执行 0.69s、三 Provider 截断 SQL 集成 0.35s。此前 `go vet ./...` 无诊断。主 agent 已读取报告并核对计数，命令及本机报告位置见 Review 005。这是新回归测试及原有自动化范围的通过，不补足下列尚未执行的验收项。

原始本机报告存放于 `$TMPDIR/unichatgo-async-reports/`，脚本私有权限并脱敏。仓库保留命令、失败历史及可复现脚本，不提交凭据/原始日志。

## Regression follow-up and review 005

本轮修复与独立复审详见 [Review 005](../design-review/review-005-regression-fixes.md)，包含未跟踪新增测试和相关文档。

- legacy 空历史统一为 `[]`，前端仅在已确认 legacy 模式兼容显式 null；实际 store 和后端 HTTP 回归证明首次发送正常。
- 图片预算改为有上限的 tile 预留，保留默认 TPM 200000 和全部配额保护；普通 512px 图片实际执行完成。确定超预算的输入在 owned-session 受理事务内返回 422，并回滚全部受理写入；同 key 重放仍优先返回原凭证。Worker 再检查增长后的完整上下文，不承诺任意 Provider 的 tokenizer 上界。
- 列表初始化状态独立于 capabilities，重新连接重试失败的列表加载；独立 review 额外发现并修复旧错误残留、迟到响应复活已删除会话、创建/导航竞争隐藏恢复入口。
- 完整类型正确的 `length` / `max_tokens` / `MAX_TOKENS` 明确终止为 `failed/provider_output_truncated`；不保存部分成功回复、不重试、不隔离会话。传输中断或不完整响应仍为 uncertain。ADR-004 和运行说明同步修订。
- 独立 reviewer 还纠正了预算预检方案中的幂等并发窗口；最终在同一受理事务中检查胜出凭证与预算。重要改动由非实现 agent 复审，当前本轮范围内无剩余 P0/P1/P2；这不等于整体 M1-M6 已无缺口。

继续保留 **NO-GO**。以下任务没有因本轮回归修复而完成；它们也不是等待用户授权才能推进的事项。

## Remaining blockers / next work

以下仍可继续开发/验证，**不是需要用户授权的外部阻碍**；若本轮因上下文/执行长度停止，应从这里继续，而非标为完成：

1. M3.5：完整标题/摘要/Agent 多轮调用的 durable 子调用边界、配额、异常恢复、工具副作用协议及实现。当前安全的单次 HTTP 路径显式拒绝不支持的附件/输出，不能代替原业务功能。
2. M5.B：PDF/Office 解析、工具读取/摘要、历史附件再次引用与保留的等价行为。迁移工具保存字节不等于功能迁移；历史 API 也尚未投影全部附件 metadata。
3. M6：实际浏览器断连/刷新/多标签全链路；完整 DB/Redis 服务不可用与恢复、容器持久存储重启/损坏/备份还原实验；目前 SQL statement fault/stream DEL 只证明对应范围。
4. M6：同会话热点/多用户/长回复/图片任务 P95/P99、内存、数据库负载和成本模型的测量报告；容量参数仍是保守默认值，不是生产 SLO。
5. M6：固定用户/会话灰度的部署级路由策略、实际兼容二进制回滚演练、跨主机存储验证和告警接入。旧 writer 必须排空，不能同时写新会话。
6. 覆盖仍不足：最终 ACK 前进程级 crash（现有仓储终态重投拒绝再调用测试，不等价直接重复 Finish 事务或该窗口的进程 SIGKILL）；物理断电 RPO；外部 Provider 计费/query/idempotency 能力。不承诺上游 exactly-once。

当前 release gate：**NO-GO**。除执行长度检查点外，无新增凭据/生产权限请求。保留隔离容器便于复验；没有擅自删除测试卷或生产数据。用户若不再需要测试服务，可按 runbook 显式停止；本轮不因本地通过声称生产上线。
