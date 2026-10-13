# Issue 001: 请求幂等、会话串行与多实例水平扩容改造

| 项目 | 内容 |
| --- | --- |
| 状态 | 待审阅 / 方案草案，尚未实施 |
| 类型 | 架构改造 Epic，可按阶段拆分为实现 issue |
| 优先级 | P0: 正确性与租户隔离；P1: 多实例上线前置条件；P2: 吞吐优化与运行治理 |
| 编写日期 | 2026-10-11 |
| 代码基线 | `a9134d1`，本轮开始时工作区无已有修改 |
| 输入依据 | 用户提供的调研结论，以及本轮对后端调度、API、存储、附件和前端流式处理代码的静态核对 |
| 本轮范围 | 仅产出决策文档，不改业务代码，不部署基础设施 |

> 后续审阅进展: 已在 `docs/design-review/` 接受“SQL-first + 轻量 Outbox + Redis Streams pull + 最小持久凭证”框架，以及前后台执行解耦、最终历史从 MySQL 恢复。本文保留最初方案比较作为背景；其中“先 MySQL 任务队列”的原始推荐不再是当前选定路线。当前决策以该目录的 README 和 ADR 为准，尚未开始实施。

## 1. 审阅摘要

当前 Worker Pool 解决的是**单进程执行并发上限、入口排队和按用户轮转派发**，不等于业务请求幂等、同会话串行或集群调度。调研中的“用户轮询”应理解为 Dispatcher 的 round-robin，不是已有可恢复的客户端任务状态轮询协议。

问题的核心不是 Worker 数量不够，而是正确性仍依赖当前进程：幂等表、会话历史、任务和回调均绑定本机；数据库消息、模型执行及 SSE 连接之间缺少持久请求状态。

**建议方向：先建立数据库事实源和请求生命周期，再决定是否引入独立任务 MQ。**

1. 立即补齐幂等命中前的权限校验、同会话完整生命周期串行、稳定的请求标识，以及失败场景测试。
2. 第一版多实例方案优先考虑“共享 MySQL 持久任务表 + 短事务领取 + lease/fencing + Redis 全局配额 + 对象存储”。先复用现有技术栈，不把引入 MQ 当作可靠性的唯一途径。
3. 如果明确需要 Gateway/Worker 独立伸缩、突发流量缓冲或已有 MQ 运维能力，则选择“事务 Outbox + Redis Streams”，但仍保留数据库请求状态、会话顺序及最终写入约束。
4. 无论选哪条路线，全局配额、共享附件、故障恢复和最小可观测性均是**多实例上线门槛**，不能等到扩容以后再补。

**不能承诺的边界：**本地结果可以做到一个逻辑请求至多提交一次；如果上游 LLM 不支持可靠的幂等键或结果查询，则无法仅靠数据库锁/MQ 保证故障情况下只调用、只计费一次。必须显式处理“上游结果不确定”，而不是盲目自动重试。

## 2. 已核对的现状与影响

以下定位对应上述基线；后续代码调整后应以符号名重新定位。

| 问题 | 源码证据 | 影响 |
| --- | --- | --- |
| 同会话任务可同时进入不同 Worker | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/dispatcher.go:98`，`dispatchOne` 派发后没有 session in-flight 标记 | 两次生成读取相同旧上下文，结果和缓存顺序不一致；按用户轮转不提供会话互斥 |
| 幂等仅覆盖同 Handler 内的执行中请求 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:519`，`beginIdempotencyEntry` 使用内存 map；`:531` 的完成路径删除条目 | 执行结束后的重试、进程重启、跨实例请求都会再次执行 |
| 幂等重放绕过会话归属校验 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:378`，命中后直接重放；key 只有 `session_id:client_msg_id` | 另一登录用户若获知相同会话 ID 和 client ID，可能在执行期间命中别人的结果；路径 user 校验不能代替 session 归属校验 |
| 用户消息先写入，任务后入队 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:394`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:226` | 缺少 Provider Key、入队失败或断连时，可能留下没有可恢复任务的用户消息 |
| 最终持久化在 HTTP Handler，不在 Worker 完成事务内 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:446`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:499` | Worker 已更新缓存并返回，Handler 才写回复；仅锁 `handleStream` 仍可能在前一回复提交前放行下一轮；断连还可能使最终写入失败 |
| 数据库无请求实体和幂等唯一键 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/storage/database.go:152`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/models/message.go:15` | 无法判断请求已受理、已执行还是结果未送达；无法用数据库兜底重复提交 |
| 缓存不是带版本的会话事实源 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:334`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/redis.go:98` | Redis 历史以整份覆盖方式更新，本机会话 ready 后复用历史；即使增加跨实例锁，也可能读取另一实例更新前的旧上下文 |
| 历史拼装存在多处追加 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:475`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/ai/service.go:125` | Manager 已将当前消息加入传入历史，AI Service 又追加一次；冷启动 DB 历史还可能已含当前消息，必须统一拼装规则 |
| 模型容量仅受本机 Worker 数量间接控制 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/pool.go:158`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/config/config.go:29` | 多实例会叠加上游请求；没有共享 RPM/TPM/并发准入，标题、摘要和 Agent 多轮调用也消耗配额 |
| 附件依赖本地文件及本地清理器 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:783`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/assistant/clean_assistant.go:38` | A 上传、B 执行时文件不可见；每实例扫描共享元数据但删除本机文件，会产生错误清理语义 |
| SSE 发送累计全文，且绑定原 HTTP 请求 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/ai/service.go:144`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:407` | 固定大小 chunk 下传输量近似二次增长；浏览器断连与后台执行生命周期耦合；没有持久 request ID 和续传游标 |
| 存储和运行入口尚未完成集群化约束 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/storage/database.go:15`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/main.go:27` | 已支持 MySQL，但默认仍是 SQLite；没有显式连接池预算；启动迁移只有建表语句，不能承担此次增量变更 |

### 测试结论的边界

- 用户提供的调研称 `go test -race ./...` 通过；**本轮没有重新执行该命令**，不把该说法当成本轮实测结果。
- `TestDispatcherJobOrder` 先同步执行 first，再同步执行 second，不能验证两个请求竞争时的串行性，见 `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager_test.go:133`。
- `TestDispatcherQueuesWhenWorkerBusy` 虽然启动了两个 goroutine，但没有在释放第一个前断言第二个未进入模型，见 `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager_test.go:196`。
- 当前消息接口队列满时已经开始 SSE，返回的是 HTTP 200 下的 `ack + error`，不是消息接口的 HTTP 429；现有测试明确覆盖此行为，见 `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers_test.go:384`。
- Race detector 检测内存竞争，不证明业务顺序、租户隔离、跨进程幂等或故障恢复正确。

## 3. 目标、不变量与非目标

### 必须满足的不变量

| 编号 | 验收不变量 |
| --- | --- |
| I1 | 在约定保留期内，`(user_id, session_id, client_msg_id)` 唯一映射一个 `request_id`；相同 key 不产生第二条逻辑请求和第二条用户消息 |
| I2 | 相同 key、不同语义 payload 返回 409；不能静默复用其他内容的结果 |
| I3 | 同一 session 任一时刻至多有一个有效执行所有者；按服务端受理序列推进，不把网络到达时间、客户端时间或 MQ 接收顺序当作顺序依据 |
| I4 | 序号 N 的模型输入只含允许纳入上下文的前序已提交轮次及 N 自身一次，不含 N+1 等未来排队输入 |
| I5 | 返回可靠受理成功前，用户消息与可恢复任务必须已原子提交；拒绝受理不留下聊天消息和任务半成品 |
| I6 | assistant 回复、请求完成状态、会话版本/执行游标在同一事务提交；过期 Worker 不能写入或发布有效完成结果 |
| I7 | 实例数变化不改变用户和 Provider 的集群配额；附件和请求状态不依赖原实例存在 |
| I8 | 任务查询、幂等命中、SSE、取消及附件访问均执行用户/会话归属校验，不因知道 ID 而获得访问权 |

I3 限定的是有效所有者和可提交结果。旧 Worker 与外部 Provider 之间的调用在网络分区后可能仍未终止，fencing 无法撤回已发出的外部请求。发现这类情况应进入不确定态并阻止盲目接管。

### 第一版非目标

- 不承诺端到端 exactly-once 模型执行或计费；不默认支持跨地域双活。
- 不因“将来可能高并发”立即引入 Kafka、Kubernetes 自动扩缩或多个独立微服务。
- 不要求每个 token 永久写入 MySQL；优先保证最终结果、可恢复状态和有限期事件/快照。
- 不承诺尚未压测的 QPS、并发数、P95/P99。容量目标应在第 12 节基线实验后确定。

## 4. 方案比较与建议

| 路线 | 核心实现 | 优点 | 代价 / 适用边界 |
| --- | --- | --- | --- |
| A: MySQL 持久任务表 | 受理事务写 `chat_requests + messages`；Worker 通过短事务领取可运行会话头任务；Redis 管全局配额 | 不新增任务 MQ；同事务即可保证任务存在；可以先同二进制，再分离进程部署 | 扫描、索引、清理和公平调度需设计；需压测 DB 负载；MySQL 8 的锁定读取能力须明确版本 |
| B: Outbox + Redis Streams | 同事务写请求、消息和 outbox；Publisher 发布；Consumer Group 消费；DB 控制顺序、lease 和最终提交 | 复用现有 Redis 经验；便于独立 Worker 和削峰；分离投递与执行 | 需要处理重复发布、PEL 回收、ACK、重试/死信、Redis 持久化及事件保留；不能把缓存 Redis 的运行标准直接照搬 |
| C: 独立专业 MQ | Outbox + RabbitMQ / Kafka / 其他成熟任务系统 | 当吞吐、路由或团队平台能力确有需求时有价值 | 新增运维及故障面；仍不能替代请求表、fencing、用户配额和会话顺序校验 |
| 仅加 Redis 锁 / 粘性会话 | 保持本地队列与回调，增加锁或实例路由 | 改动表面较小 | 无法原子保证消息与任务、无法恢复本地丢失任务、锁过期会重叠执行；不作为目标方案 |

**暂定推荐 A，B 为有明确需求时的升级路线。**这不是认定 A 的吞吐一定够用，而是在缺少目标流量和压测数据时避免过早增加投递链路。如果评审直接选择 B，跳过 A 的数据库轮询实现，但不跳过共同的数据模型和一致性约束。

路线 A 的请求表本身就是持久队列，不必为内部任务领取额外引入 Outbox。只有需要向另一个系统可靠发布事件时，才引入 Outbox；不能写完数据库再“尽力发一下 MQ”作为可靠方案。

## 5. 目标链路与模块职责

```text
Browser -- POST request --> LB --> API 集群
                                 |
                                 +-- MySQL 受理事务
                                     user_message + chat_request
                                     (+ outbox，仅路线 B)
                                             |
                    A: DB 领取 <--------------+
                    B: Publisher -> Streams -+
                                             v
                                      Model Worker 集群
                                      | 会话头任务 + lease/fence
                                      | 用户准入 + Provider 配额
                                      v
                                      上游 LLM
                                      |
                                      +-- MySQL 最终提交事务
                                      +-- 共享事件/快照 --> SSE Gateway --> Browser

Browser -- GET status / GET events --> 任意 API/SSE Gateway
上传服务 / 任意 Worker <-----------> 对象存储
```

- API: 身份及资源归属验证、幂等语义、可靠受理、状态查询，不持有任务正确性状态。
- 调度器/Worker: 公平领取、会话序列、lease、重试、调用上游、原子提交结果；本地 Pool 保留为执行器和内存保护，不再是任务事实源。
- MySQL: 请求、消息、会话顺序、附件引用和最终状态的事实源。
- Redis: 全局配额和有限期事件/缓存；选 B 时增加任务投递职责，并与可驱逐缓存隔离资源预算。
- SSE Gateway: 将已鉴权的共享事件转发到连接；不负责完成任务落库，不能依赖某台原 Worker。
- 对象存储: 附件事实源；本地目录仅为可丢弃的临时下载/解析缓存。

这些是逻辑职责，不要求第一版每一项都变成独立服务。A 路线也支持 API 与 Worker 分开部署，不应将“无 MQ”等同于“只能单进程”。

## 6. 持久幂等与可靠受理

### 6.1 建议的数据模型

以下为逻辑模型，不是可直接执行的迁移 SQL。字段类型、索引长度、外键删除策略和 MySQL 版本在实施前确定。

| 实体 | 建议字段 / 约束 |
| --- | --- |
| `chat_requests` | `request_id`、`user_id`、`session_id`、`client_msg_id`、`payload_hash`、`session_sequence`、`user_message_id`、`assistant_message_id`、`provider`、解析后的 `model`、非明文 `credential_ref`、生成参数/配置版本快照、`status`、`attempt`、`next_attempt_at`、`deadline_at`、`lease_owner`、`lease_until`、`fencing_token`、`cancel_requested`、`error_code`、创建/开始/完成时间 |
| 幂等索引 | `UNIQUE(user_id, session_id, client_msg_id)`；client ID 作为不透明值，限制长度并使用大小写明确的比较语义，例如二进制排序规则 |
| 顺序与扫描索引 | `UNIQUE(session_id, session_sequence)`；按实际领取 SQL 验证 `(status, next_attempt_at)`、`(status, lease_until)` 及会话头任务索引，不能仅凭列名推定性能 |
| `sessions` 扩展 | `next_request_sequence`、执行游标、`active_request_id`、单调递增的 session fencing epoch、lease、`history_version`；可以拆到独立运行态表，保持一会话一行的竞争锚点 |
| `messages` 扩展 | 可空 `request_id`、轮次序号、消息种类/轮内序号；对请求关联的 user/final-assistant 分别加唯一约束，历史消息和多条 system/tool 消息不能被一个过宽唯一键误伤 |
| `request_attachments` | 请求与附件版本的冻结引用，包括 file/object 标识和顺序；不能执行时重新解释成“当时会话里有哪些附件” |
| `outbox`，仅 B | `event_id`、`request_id`、事件类型/版本、最小 payload、`published_at`、发布 lease、`attempt`、`next_attempt_at`；初次 `ChatRequested` 事件有唯一业务键 |

第一版应冻结影响请求语义的输入：正文、解析后的 Provider/model、参数、按约定排序的附件 ID/版本及服务端配置版本。`payload_hash` 对这些规范化字段计算，不包含 API Key 明文或不稳定时间。默认模型变更不能改变已受理请求的执行目标。

### 6.2 受理流程

1. 验证登录态、CSRF、session 归属、输入格式；**即使幂等命中也不能跳过权限验证**。
2. 在用户/session 命名空间查找已有请求。命中且 hash 一致时返回已有 ID/状态；hash 不同返回 409。重放已完成结果不应因为附件之后过期或 Provider Key 已撤销而重新失败，但用户必须仍有访问权限。
3. 未命中时检查附件归属/可用性、Provider 配置及凭据是否存在，并检查新请求的受理额度。认证、滥用防护可在此之前执行，业务重复请求不重复占用队列名额。
4. 开启短事务，按固定锁顺序锁定需要的用户配额行及会话行；再次检查重复 key、会话状态和容量；分配 `session_sequence`，插入用户消息、请求和冻结附件引用。B 同事务插入 outbox。
5. 唯一键冲突时回滚整笔事务，再读取胜出的已有请求并比较 hash，不能保留失败事务创建的用户消息或递增计数。
6. 提交成功才返回 `202 + request_id + status_url + events_url`。提交结果不明时客户端使用同一 key 查询/重试，不生成新 key。

用户 `max_queued` 等要求严格控制的受理计数应由同一事务内的持久计数/锁定查询保证；Redis 可做前置快速拒绝和请求频率控制，但不能用非原子的“COUNT 后 INSERT”兑现硬上限。

### 6.3 重试与保留期语义

- 网络重发: 同一 `client_msg_id`，只查询/重放已有请求；不得重新插入消息。
- Worker 可安全重试: 保持 `request_id` 与 `session_sequence`，增加 attempt，不产生新的用户消息。
- 用户显式“重新生成”: 新 client ID，可记录 `retry_of_request_id`；不是旧 key 重放。前端必须区分“查询原结果”与“发起新生成”。
- 终态请求也参与幂等，不能在完成时删 key。建议初版随会话保留最小幂等记录；若以后只保留有限窗口，必须公布窗口并保留过期 tombstone/明确拒绝规则，不得静默把旧 key 当新请求。
- 删除用户/会话走显式删除流程；已删除资源返回不可访问，不因重试而重建。幂等摘要的保留也需服从删除策略。

## 7. 会话顺序、上下文与执行所有权

### 7.1 本机修复不能只包一把模型调用锁

- 对不同 client ID 的同会话任务是“排队串行”，不是合并成一个结果；不能直接套用按 session 合并返回值的 `singleflight.Group`。
- 临时本机方案应覆盖初始化/历史读取、模型生成、最终回复落库和缓存版本更新后再放行下一轮。当前 Worker 返回后 Handler 才持久化，必须一起调整边界。
- 调度器只将可运行 session 放入 ready 集合，不能让大量等待同一 session 锁的任务占满所有 Worker。
- 保留用户之间的轮转；用户的多个不同会话可并行，但共同受该用户 `max_running` 和 `max_queued` 约束。

### 7.2 集群领取与 fencing

1. 服务端受理事务分配递增序列；每次只允许领取该 session 尚未解决的头任务。不能先跳过一个被锁住的请求，再执行同 session 后面的请求。
2. A 可用 MySQL 8 的短事务锁定领取；如使用 `SKIP LOCKED`，锁定/筛选单位应保证会话头约束。B 的投递也必须重新检查数据库头任务，MQ 顺序不是事实源。
3. 同一领取事务设置 request/session 的有效 owner、lease 和新的 session fencing epoch；用户运行额度原子预约，所有更新检查影响行数。
4. 提交事务后再进行耗时工作，绝不能在调用 LLM 期间持有数据库行锁或事务。
5. Worker 定期续租；续租失败立即停止继续发送上游调用，取消现有调用并停止发布可见结果。最终事务同时校验 request、session active owner、fence、lease 及取消状态。
6. 最终 assistant 消息、请求 `completed`、会话历史版本与推进游标原子提交；提交后才发 `done`。重复提交靠唯一键及 CAS 无害化，使用数据库实际生成的消息 ID。

**只锁 request 不够：**两个不同 request 仍可能属于同一 session。**只检查 lease_owner 也不够：**过期进程恢复后可能继续写；所有有副作用的持久写入需要 fencing，包括标题、文件摘要和缓存版本。事件也携带执行 epoch，拒绝旧执行的输出。

### 7.3 上下文只从已提交事实构建

- 第一版优先在每轮执行前从 DB 读取合法前序轮次及当前消息，按明确的轮次/轮内顺序组装；移除 Manager 与 AI Service 各自追加当前消息的双重职责。
- 排队时即保存用户消息后，不能再用“取 session 全部 messages”组装输入，否则 N 会看到未来的 N+1。UI 展示顺序也不能只按 `created_at`，否则可能展示为 `user1, user2, assistant1, assistant2`。
- 如缓存历史，以 `history_version` 验证命中；缓存未命中、版本不符或失效通知丢失时回源 DB。Pub/Sub 仅作加速，不参与正确性证明。
- 失败/取消轮次是否进入后续上下文须统一。建议失败/取消轮次保留在 UI，但不进入模型有效历史；正常终态后允许推进，不确定态必须阻塞会话后续执行，等待处理。
- 标题与附件摘要也属于可重试副作用。可独立记录操作唯一键，例如 `(file_version, summary_config_version)`，并与有效 fence 关联；不要只对最终 assistant 消息做去重。

## 8. 状态机、恢复与 MQ 边界

### 8.1 建议最小状态机

```text
queued -> running -> completed
  |          |
  |          +-> retry_wait -> queued      # 已确认可安全重试
  |          +-> failed                    # 已确认的永久失败 / 重试耗尽
  |          +-> uncertain                # 上游可能已执行，结果不可确认
  |          +-> cancelled / expired      # 已确认停止，或尚未产生外部执行
  +-> cancelled / expired

uncertain -> completed                    # 上游结果被可靠查回并提交
uncertain -> failed                       # 确认无法恢复并结束
uncertain -> retry_wait                   # 显式授权承担重复调用风险，或证明未执行
```

- 第一版以 `queued` 表示数据库已可靠受理，不额外暴露一个含义模糊的 `pending`。B 的“尚未发布 MQ”由 outbox 表示；Publisher 不得把已 running/completed 的任务改回 queued。
- `retry_wait` 仍占据会话头部，不能让后续请求超车；明确最大 attempt、截止时间和人工处理入口。
- 浏览器断连仅取消 SSE 订阅，不自动取消任务。显式取消先写 `cancel_requested`，由 Worker 停止并确认；无法确认上游停止时进入 uncertain，而不是假装取消已无副作用。
- 任务超时与排队超时分开配置；不要让 Gateway 的连接超时成为持久任务的生命周期。
- 会话删除先阻止新受理并取消/隔离在途任务，再清理消息/附件；旧 Worker 不得在删除后写回或重建数据。

### 8.2 故障策略

| 故障点 | 期望处理 |
| --- | --- |
| DB 事务提交前崩溃 | 全部回滚；相同 client ID 可重新受理 |
| 提交后、HTTP 应答前崩溃 | 相同 key 读取同一 request；A 从 DB 继续领取，B 补发 outbox |
| LLM 调用前 Worker 崩溃 | lease 到期且确认未开始外部调用后重新领取 |
| LLM 已受理，Worker 丢失响应或崩溃 | 如 Provider 支持则用稳定调用标识查询/恢复；否则标记 uncertain，不自动重复扣费 |
| DB 完成事务提交后、done/MQ ACK 前崩溃 | GET 状态返回已完成；重复投递不再调用模型；终态事件可补发 |
| 旧 Worker 在接管后恢复 | fence 校验拒绝 DB 写入；旧 epoch 事件被过滤；不能依赖旧进程自觉退出 |
| Redis 配额服务不可用/状态丢失 | 暂停新的上游调用；恢复前重建或保守隔离可能仍在途的许可，禁止退回每实例无限制执行 |
| DB 不可用 | 不确认新请求受理，不允许无法确认所有权的 Worker 启动调用；已返回结果尽力保留并进入可恢复路径 |

“已开始外部调用”的标记与真实网络发送之间无法原子提交：先记后发可能误判，先发后记可能重复。因此恢复策略应保守地把该窗口视为 uncertain，不靠一个布尔字段声称解决 exactly-once。

### 8.3 选择 B 时的额外要求

- Publisher 采用“发布成功后标记已发布”；两者之间崩溃可能重复发布，消费端必须幂等，不允许先标记后发送造成丢任务。
- Redis Streams 的多消费者不会自动保证同一 session 串行完成；按 session 分区只是一种辅助路由，仍需第 7 节的头任务和有效 owner 校验。
- ACK 在最终 DB 提交后，或在任务已被可靠移交到持久重试调度后执行；不能领取即 ACK。重试等待、非头任务和全局限流等待不得变成忙循环重投。
- PEL 回收只能发现候选失联消息，不能代替 DB lease 和上游不确定性判断；不能回收后立刻无条件重新调用模型。
- 配置持久化、保留策略、禁止任务数据驱逐及恢复演练；设置有界投递重试和死信诊断，不在死信中放凭据或完整敏感附件。
- 定期扫描已受理但长期未运行/未终态的 DB 记录，即使 MQ 中已丢失投递，也能安全重建调度信号。任务存在性不只依赖 Redis 的持久性承诺。

## 9. 全局容量、背压与 Provider 治理

### 配额分层

| 层级 | 必需控制 | 语义 |
| --- | --- | --- |
| API 入口 | 请求速率、请求体大小、连接数 | 防滥用，幂等查询也受基础安全限制 |
| 用户 | `max_queued`、`max_running`、受理频率 | 集群总量；持久队列不是无限接收承诺；限制一个用户占满公共资源 |
| 上游配额域 | 并发、RPM、TPM、冷却时间 | 按实际 Provider 账户/项目/凭据组及模型限额组织，而非仅按 Provider 名称 |
| 本机 | Worker 数、内存、文件解析并发、SSE 缓冲 | 资源保护；不能替代集群配额 |

当前支持用户自带 Provider Key，因此只设置一个 `openai` 总桶会错误地让互不相关的账户互相争抢配额。建议同时配置平台总预算与上游实际 quota scope；用受保护的凭据引用/配额组 ID，不把 API Key 明文写入 Redis key、日志或 MQ。

- 全局并发采用有 owner/lease 的许可，RPM/TPM 采用原子令牌预留与结算；Redis Lua/事务操作需满足所用部署的同槽约束。
- 获取许可遵循统一顺序，失败时释放已预留资源并持久化下次可运行时间，不持有 DB 事务，也不让大量 Worker 睡眠等待配额。
- TPM 根据输入估算和输出上限预留，完成后按可得 usage 结算；失败时不能把 Provider 可能已消耗的额度一律退回。
- 应在**每次实际模型 API 调用**前准入，覆盖标题、附件摘要、主回复和 ReAct 多轮调用，而不只是每个聊天任务一次。第三方 SDK 隐藏重试需要禁用或纳入同一预算。
- 429 尊重上游 `Retry-After`，使用有上限的指数退避及 jitter；只有能确认安全的失败才重试，不能将所有流式中断都当成可重试错误。
- Provider 故障隔离使用独立并发预算/队列选择，不让一家故障占满全部执行槽；熔断半开探测也必须是有界的。
- Redis 故障或重启后，许可账本可能无法反映还在运行的外部调用；采用暂停、对账和保守等待策略。承认无上游配合时无法给网络分区期间的物理在途数提供绝对保证。

**排队策略：**用户超限返回 429；系统拒绝受理返回 503 或约定的 429，并附 `Retry-After`。已可靠受理的任务保持 queued/retry_wait，不能返回“未受理”诱导新 key 重发。最终状态与受理失败是两类不同错误。

## 10. API、SSE 与前端协议演进

### 10.1 建议接口，路径待实现时定版

可在现有 `/api/users/:id` 鉴权前缀下增加版本化接口，而不是直接改变旧 SSE 语义：

| 操作 | 建议契约 |
| --- | --- |
| `POST /conversation/requests` | 使用现有正文/附件字段及必填 client ID；新受理返回 202；同 key 同 payload 的非终态返回 202、终态返回 200；均返回同一 request ID |
| `GET /conversation/requests/:request_id` | 返回 status、sequence、错误码、重试/取消状态及已提交消息；也作为断连后的最终核对接口 |
| `GET /conversation/requests/:request_id/events` | 可重连 SSE；支持事件游标或快照重置；连接只订阅，不再次启动模型 |
| `POST /conversation/requests/:request_id/cancel` | 幂等记录取消意图；如果已完成，返回实际终态，不伪造取消成功 |

所有新接口沿用认证边界；写接口保留 CSRF。SSE 用同源 Cookie 或受控授权方式，不把长期 token 放在 URL。原生 EventSource 与 fetch-SSE 的鉴权/续传能力应一起评估。

### 10.2 增量和恢复语义

- 事件区分 `queued`、`running`、`delta`、`snapshot/reset`、`done`、`error`；事件携带 `request_id`、`execution_epoch`、`sequence`，SSE `id` 可编码为 `epoch:sequence`。
- `delta` 只携带增量文本。前端对相同 epoch/seq 去重；发现缺口不直接继续拼接，应请求重放或快照。新执行 epoch 先 reset，不能把重试文本接在旧半句后面。
- `done` 只在 DB 终态提交后发送，包含最终消息 ID；接收不到 done 时，前端通过 GET 核对，不能把“连接关闭”视为生成成功。
- 建议第一版支持“有限期事件回放 + draft 快照兜底”，不承诺永久逐 token 重放。Redis Streams 用于事件回放时，各 Gateway 按游标独立读取，不能用竞争消费组让事件只到达某一个订阅者。
- 若选择更轻量的 Pub/Sub，需明确其允许丢事件：先订阅并缓冲，再读带 cursor 的快照，过滤重复后转发；定期 checkpoint/状态核对修复中途丢包及终态通知丢失。
- 快照初始可按 0.5-2 秒聚合，频率是待压测参数；快照更新也检查 epoch，不能让旧 Worker 覆盖新执行。快照和 cursor 必须对应同一已持久前缀。
- 游标超出保留窗口返回 reset/snapshot；订阅读取与状态读取之间的竞态必须覆盖，不能只做“先读 DB，再订阅”并假设中间没有事件。
- 限制单连接缓冲和用户连接数，慢客户端断开后可恢复，不能反向阻塞 Model Worker；配置心跳、代理缓冲和 LB 空闲超时。

前端当前在 `/Users/moyu114/Documents/agent_unity/UnichatGo/frontend/src/store/session.js:249` 用累计正文覆盖文本。改 delta 必须同时修改解析器及 store；旧累计流和新增量流以版本/事件名区分，不能只改后端。客户端保存待确认请求的 client ID，重发复用它，并按消息 ID 合并 ack/done，避免 UI 自身重复插入。

## 11. 共享附件与生产存储

### 11.1 附件

- 新上传写入 S3/COS/MinIO 等共享对象存储；DB 保存 `storage_backend`、`object_key`、版本/校验和、大小、MIME、归属和生命周期状态，不保存可跨主机解释的本地绝对路径。
- 以随机对象 key 防覆盖；展示文件名与对象定位分离。对象不可公开读，访问时校验请求与文件归属，预签名 URL 只短期有效且不作为持久身份。
- 对象上传与 DB 无法组成单事务：先完成对象，再提交 active 元数据；失败时补偿删除，残留孤儿对象由对账/生命周期回收。不能返回成功却只有半上传对象。
- 受理时冻结实际附件集合，覆盖当前“未传 file_ids 时可能读取会话缓存附件”的隐式行为；明确空数组是“无附件”还是“复用附件”，不能重试时变成另一个集合。
- 活跃请求固定附件引用/延长保留期；清理不能删掉仍被 queued/running/retry_wait/uncertain 请求使用的对象。对象生命周期策略同样要尊重引用保护，不能只保护 DB。
- 清理采用单独任务的 lease 或按对象 CAS 领取，使删除可重试且幂等；不要求绝对只能有一个清理进程，但不能让多个 API 实例无协调地扫描删除。
- 更新所有读取链路，而不只是上传接口：图片 `os.ReadFile`、文件摘要 loader、`temp_file_reader` 工具、删除用户/会话及过期清理。需要本地 URI 的库可下载到任务私有临时目录，结束后清理。
- 存储容量配额也需并发安全：当前“先统计 usage，再上传”的方式不能作为多实例硬上限；使用原子预留和失败释放/超时对账。

旧附件迁移使用双读、仅写新后端：由能访问旧文件的源实例/离线任务复制、校验 hash/大小，成功后更新位置。全部验证前禁止删除旧文件；跨主机 Worker 上线前消除无法读取的旧附件，不能靠粘性路由掩盖。

### 11.2 数据库与密钥

- 多实例部署统一共享 MySQL；SQLite 保留为本地开发/单实例选项，不以共享 SQLite 文件作为生产集群方案。
- 显式配置最大打开/空闲连接、连接生命周期及查询超时；按 `实例数 x 每实例连接池` 加上 Publisher/迁移等连接预算评估 DB 上限。
- 引入版本化迁移和单次迁移任务；当前 `CREATE TABLE IF NOT EXISTS` 不会给已有表添加字段/唯一键，不能靠重启完成升级。
- 所有合法 Worker 使用受控、兼容的 `UNICHATGO_APIKEY_KEY`/密钥版本获取凭据；只在执行时解密，不向 MQ/事件/前端传递明文。明确排队期间密钥撤销和轮换的失败语义。
- readiness 校验数据库版本/迁移版本、共享存储和必需配额依赖，防止以错误配置进入多实例服务。

## 12. 分阶段实施清单与上线门槛

阶段是依赖顺序，不是工作量估算。每阶段可以拆 PR；是否继续进入下一阶段由验收结果决定。

| 阶段 | 可拆分工作 | 验收 / 发布限制 |
| --- | --- | --- |
| S0: 决策与复现，P0 | 确认第 14 节；添加同 session 真并发、幂等越权、完成后重试、队列满落库等复现测试 | 明确现有失败基线；确定幂等/失败上下文和未知结果语义 |
| S1: 单机正确性，P0 | 权限前置；完整轮次串行；历史单一拼装；受理/最终写入事务与持久请求唯一键；恢复扫描闭环 | 一个实例重启后仍可找回已受理请求；不同 session 可并行；此时仍不宣称可多实例上线 |
| S2: 共享执行底座，P1 | 共享 MySQL 与版本迁移；对象存储及旧附件迁移；A 的 DB 调度或 B 的 Outbox/Streams；会话 lease/fence、用户与 Provider 全局配额 | 两实例、独立磁盘环境下通过共享受理、领取、附件与配额测试；所有扩容前置能力必须齐备 |
| S3: 异步协议与恢复，P1 | 202/status/SSE；前端稳定 client ID 与增量合并；终态持久化、断连恢复、取消、不确定态处理；优雅下线 | 请求受理与原 HTTP 连接解耦；断连换实例可继续查看；完成故障注入矩阵后才开放多实例生产流量 |
| S4: 容量优化，P2 | Provider 熔断及更精细隔离、调度公平性优化、死信工具、延迟/成本压测；按数据决定 A 升 B 或专业 MQ | 不以增加 Worker 数突破上游配额；自动扩缩需设上限并基于 queue lag、资源及配额余量 |

S1 的持久请求实现必须同时具有恢复途径，不能只加唯一索引后留下永久 running。S2 的基础全局配额不能推迟到 S4；S4 是在已有硬保护上增加治理。

### 建议实现落点

| 现有模块 | 改造职责 |
| --- | --- |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go` | 提取受理/查询/取消 API；移除正确性所依赖的本机幂等 map 和 Handler 最终落库职责 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go` | 拆分调度、执行及结果提交；不再携带跨进程不可序列化的回调/Context 作为任务事实 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/dispatcher.go` | 单机阶段按 session ready/user fair 调度；共享阶段作为受全局准入约束的本地执行器 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/ai/service.go` | 无歧义输入历史、delta 输出、明确区分 EOF 与流错误、上游调用级准入 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/storage/database.go` | 版本迁移、连接池配置；新增请求仓储与事务服务承接幂等/领取/提交逻辑 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/redis.go` | 将缓存与配额/事件职责隔离；缓存版本化，不复用 TTL 缓存语义存正确性状态 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/assistant/session_assistant.go` | 请求关联的历史读取、附件引用与状态；图片/工具/清理链路同步引入存储抽象 |
| `/Users/moyu114/Documents/agent_unity/UnichatGo/frontend/src/api/sessions.js`、`/Users/moyu114/Documents/agent_unity/UnichatGo/frontend/src/store/session.js` | 受理与订阅解耦、续传、请求状态持久标识、增量及重复事件处理 |

### 最小运行指标与下线

- 从 S1 起记录 request ID、状态迁移、attempt、session sequence 和 fence；日志不包含 Key、完整 prompt 或附件内容。
- 多实例上线前具备 queue depth/oldest age、等待 P95/P99、TTFT/总时长、失败/uncertain 数、lease 超时、Provider 429/配额使用、DB 池饱和、SSE 重连及对象读取失败指标。
- 高基数 request/session/user ID 用日志或 trace，不直接作为无界监控标签。健康告警与不确定请求须有明确人工处理入口。
- 优雅下线: Gateway 停止新受理并允许客户端重连；Worker 停止领取，继续续租并有界排空。只有确认调用未开始或已停止时才能释放运行所有权；不得直接释放 lease 让另一实例重叠调用。

## 13. 验收与迁移验证矩阵

所有关键并发测试使用 barrier/channel 或可控假时钟，断言模型进入次数、输入内容及数据库最终行数；不以 sleep 后“看起来没问题”或仅 `-race` 通过验收。

| 场景 | 必须断言 |
| --- | --- |
| 同 key 跨两个 Handler/进程并发提交 | 一个 request、一条 user 消息、同一 request ID；正常无故障下只进入一次生成流程 |
| 完成后重发、重启后重发 | 返回原状态/原消息 ID，不再调用模型 |
| 同 key 不同正文/model/附件 | 409，无第二次副作用；大小写及规范化规则一致 |
| 另一用户使用已知 session/client/request ID | 不泄漏 ack、结果、状态、附件或事件；命中幂等也必须拒绝 |
| 第一轮模型阻塞时提交第二轮同 session | 第二轮尚未进入模型；第一轮最终提交后才运行，且上下文含第一轮回复 |
| 两个不同 session 并行 | 在用户/Provider 配额允许时均能进入模型，不退化为全局串行 |
| 多轮先入库后执行、冷缓存及跨实例切换 | 无未来消息、无重复当前输入，顺序确定；陈旧缓存/丢失失效消息不改变结果 |
| 队列/用户额度满、凭据缺失、事务失败 | 拒绝前不创建孤立消息；同 key 重试不额外消耗业务队列名额 |
| 受理提交后立即杀 API | 请求仍可查询并被调度；不依赖旧 HTTP 连接 |
| 杀 Worker、暂停到 lease 过期后恢复 | 安全窗口可恢复；旧 fence 不能写回；已发出调用的不确定窗口不盲目重试 |
| 结果提交后丢失 HTTP/SSE/ACK | 查询得到唯一最终回复，重投不重新生成；done 中消息 ID 与 DB 一致 |
| 扩容到多个 Worker、多个 Key/quota scope | 平台和实际配额域同时受控；标题/摘要/Agent 循环和 SDK 重试也计入 |
| Redis 重启/配额不可用 | 不突破保守准入策略；不会退化为每实例各放行一份配额 |
| SSE 断连、重复/乱序事件、过期 cursor、慢客户端 | 跨 Gateway 恢复后正文不重不漏或明确快照 reset；重连不触发新模型调用 |
| 显式取消、会话删除与完成事务竞争 | 返回真实终态；没有取消后提交或删除后复活；未知外部状态可见 |
| A 上传、B 执行且两者不共享本地磁盘 | 图片、摘要、工具读文件均成功；引用中的附件不会被清理器误删 |
| B 专属: 发布后未标记、重复消息、PEL 回收、MQ 数据丢失 | 重投不重复提交；DB 对账能恢复任务；非头任务不超车，限流不产生重投风暴 |

测试分层: 单元测试验证状态机/规范化；真实 MySQL 集成测试验证唯一键、事务和锁；双进程 + Redis + 对象存储验证集群行为；故障注入验证提交边界。SQLite/in-memory mock 不能替代 MySQL 并发语义测试。

容量实验使用可控延迟/限流的 mock Provider，分别测同 session 热点、同用户多 session、多用户、多附件及长流式回复。记录受理率、排队时延、内存、DB 查询量、网络字节和公平性，再确定首版 SLO 与队列/连接池预算；不直接拿真实用户 Key 做无预算压测。

### 迁移与回滚

1. 扩展式迁移先添加表/可空字段和索引，历史消息无 client ID 的保持 legacy，不伪造历史幂等记录。切换前分配确定的历史顺序边界，新请求从该边界后排序。
2. 先部署可读新旧数据但仅受控启用新链路的版本，再迁移附件和验证查询；模型调用不能双写/双执行来验证“影子链路”。
3. 按用户/会话固定分流并排空旧任务，同一 session 同时只能由一种执行链路写入。旧接口如保留，适配到统一受理服务，不继续维护另一份内存幂等。
4. 新路径稳定后再拆分部署与扩容。全过程保留数据库备份、请求对账和旧附件校验结果。
5. 回滚时先停止新受理/领取并排空或妥善保留持久任务；切回兼容新数据的版本，不能切回忽略请求表的旧 Worker，也不能删除有待执行请求的表或覆盖对象。

## 14. 待你审阅的决策项

下列为建议值，不代表已批准。容量数字和工期应在测量后填写，不在本文猜测。

- [ ] **调度路线:** 先 A（MySQL 持久任务表，推荐），还是直接 B（Outbox + Redis Streams）？若选 B，确认有持久化、回收、对账和运维预算。
- [ ] **首版部署目标:** 仅修单实例，还是至少两个 API/Worker 实例且可独立下线？推荐以双实例故障测试作为水平扩容验收。
- [ ] **并发交互:** 同会话后续输入排队（推荐），还是执行中直接拒绝？不同会话在用户额度内并行。
- [ ] **幂等保留:** 随会话保留最小记录（推荐），还是限定重试窗口？过期 key 不允许静默重跑。
- [ ] **未知结果:** 默认暂停并查询/人工确认（推荐），还是授权自动重试并接受重复计费风险？需在产品提示和计费审计中体现。
- [ ] **失败上下文:** 失败/取消轮次不进入模型历史、UI 保留并提示，后续正常推进（建议）；若要求失败即阻塞整会话，需要增加显式跳过/恢复交互。
- [ ] **断连/取消:** 断连继续任务（推荐）；取消为显式操作，无法确认上游停止时允许显示 uncertain。
- [ ] **附件后端:** 使用现有云对象存储还是自建 MinIO？确认旧文件迁移、引用保护、数据删除和成本要求。
- [ ] **配额归属:** 确认 BYOK、平台共享 Key 或混合模式，给出实际账户/项目级限额及用户公平性目标。
- [ ] **事件恢复:** 有限期回放 + 快照兜底（推荐），还是仅允许快照恢复？确定草稿保留时间和敏感内容存储政策。
- [ ] **容量目标:** 明确并发活跃用户、消息到达率、平均生成时长、排队上限、可接受等待 P95、成本预算和恢复时间目标。

## 15. 完成定义

只有同时满足以下条件，才将本 issue 的“支持水平扩容”标记为完成：

- [ ] 上述决策已记录，并将选定路线拆分为可跟踪的实现任务。
- [ ] 幂等、授权、会话顺序、上下文拼装和最终提交不依赖进程内存或粘性路由。
- [ ] 已受理请求在进程崩溃后可恢复或进入可解释的不确定态，不永久悬挂、不无声丢失。
- [ ] 双实例且独立本地磁盘时，任务、附件、全局配额和跨 Gateway 恢复通过验证。
- [ ] 故障与未知结果边界明确，没有声称不具备的 exactly-once 能力。
- [ ] 通过第 13 节关键用例，并留存实际容量结果、迁移回滚步骤及运行告警/处理说明。

当前进度: **仅完成调研核对和方案草案，所有实施与验收项均未完成。**
