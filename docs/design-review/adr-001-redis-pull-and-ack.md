# ADR-001: Redis Pull、持久化边界与完成确认

- 状态: **方案对比保留；第 2 轮已选择路线 O，详细 ACK/回收协议仍待细化**。
- 日期: 2026-10-11。
- 关联: 原 Issue 001 与本目录 `round-001.md`。
- 目的: 不预设完整 MySQL 请求表，核对 Redis 为主的轻量路线能做到什么、需要改变什么。

## 1. 用户提出的方案

引入 Redis 队列，Worker 主动拉取；尽量保留当前消息 SQL 持久化与本机内存设计，避免“所有请求进 MySQL 请求表”带来的复杂度。需要确定取任务、标记处理中、落库、返回与 ACK 的顺序。

“请求表太重”目前是待验证的设计顾虑，不是已有性能结论。完整请求表可以不作为前提；轻量唯一标识、事务调整、Redis 状态机的成本需要分别比较。

## 2. 已验证的代码事实

| 事实 | 证据 |
| --- | --- |
| 用户消息先入 SQL，然后发 SSE `ack`，再进入 Worker；不是先可靠队列受理 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:394`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:416`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:436` |
| Worker 先更新回复缓存，再返回给 Handler；助手回复由 Handler 写 SQL | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:499`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:446` |
| 当前完成后的幂等记录立即删除，不是保留完成结果的内存请求表 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:531` |
| 当前消息表无请求关联字段和相应唯一结果约束 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/storage/database.go:152`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/models/message.go:15` |
| 当前插入消息与更新会话时间是两个独立 SQL 操作，后者失败时前者可能已经提交 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/assistant/session_assistant.go:94` |
| Compose 配置启用 Redis AOF 并挂载 `/data` | `/Users/moyu114/Documents/agent_unity/UnichatGo/docker-compose.yml:28`；这只证明声明配置，不证明运行实例配置、复制/故障切换或已验证 RPO |
| Redis 客户端为单地址普通客户端，仓库未显式设置 fsync、淘汰策略和 HA 客户端 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/redis/redis.go:36`；不能用未经核验的默认值代替生产可靠性承诺 |

因此“仅把本地 channel 换成 Redis pull”不会自动修复跨实例幂等、同会话串行、落库与入队之间的窗口，或 Worker 返回后最终落库失败。

## 3. 先澄清 Redis 与 Outbox

传统 SQL Outbox 是**用户消息和待发事件在同一 SQL 事务里提交**，发布器再把事件送到 Redis。消费者用 pull 还是 push 与 Outbox 是否存在是两回事。

如果业务受理事实本来就在 Redis，也可以设计 Redis 内原子更新状态和追加 Stream 的机制，但它不能顺便把 MySQL 写入包含在同一事务里。故“Redis outbox”必须先说明**哪一份数据是受理事实源**。

当前 SQL-first 不改动时存在两个必然窗口：

```text
SQL 用户消息提交 --[此处崩溃]--> 尚未 XADD，任务丢失
XADD 成功 --[此处连接中断]--> API 不知道已入队，重发可能重复
```

Redis Lua 或 `MULTI/EXEC` 只能解决 Redis 内部的原子协调，不能单独封闭第一个窗口。

## 4. Pull 方式建议

**推荐候选: Redis Streams Consumer Group + 阻塞拉取。**

```text
Producer: XADD chat:tasks * <任务描述>
Worker:   XREADGROUP GROUP chat-workers worker-A
          COUNT <当前可用执行容量> BLOCK <有界等待时间>
          STREAMS chat:tasks >
```

- 阻塞调用是 Worker 发起的拉取，没有任务时由 Redis 等待并响应，不需要高频空轮询。
- entry 返回后已登记到 PEL，表示“已投递、待确认”，不是业务完成。不要使用 `NOACK` 绕过该机制。
- 拉取数量不能远大于实际可执行容量，否则大量任务被某个消费者占住，只是从共享积压搬到本机积压。
- `>` 读取新投递，不会自动取回所有旧 pending；需要明确启动恢复、失联检测和回收逻辑。
- `BLPOP` 直接弹出后进程崩溃可能丢失处理责任；可以自行实现 processing list，但第一版通常不值得重造 Streams 已有的 pending/ACK 能力。
- pull 提供背压便利，不自动提供 user 公平性、session 串行或 Provider 全局额度。

## 5. 正常完成的推荐顺序

这是候选协议，不是已批准代码；其成立依赖完成凭证、所有权与恢复路径。

```text
1. XREADGROUP 领取，entry 进入 PEL
2. 校验当前执行所有权、会话顺序、全局额度及已有完成凭证
3. 若已有完成凭证，不再调用模型，修复状态并确认投递
4. 否则执行模型调用，必要时向前端发送进度
5. 将最终回复及稳定请求关联写入 MySQL，提交成功
6. 条件更新 Redis 终态，并 XACK
7. 通知前端 done；通知失败通过状态/结果查询补偿
```

步骤 6 与实时通知的相对顺序不是可靠性的核心：`done` 必须以最终提交为准，ACK 不等待浏览器。终态状态必须能从已提交结果恢复；如果终态通知要求保证投递，还需可重放事件/补发机制，不能只依赖第 7 步尽力发送。

Redis 内的终态写入与 `XACK` 可在同一脚本/事务中协调，但必须设计合法状态检查、owner/epoch 校验与部署同槽条件；这仍然不与第 5 步构成跨系统事务。

轻量 SQL 凭证也要与结果在同一事务提交。用户消息同样需要唯一关联，不能只防止重复 assistant 消息。最终通知应使用数据库实际返回的消息 ID，而不是尚未关联 SQL ID 的模型临时对象。

**不要把 XACK 当作带 owner 校验的完成接口。**它确认 entry 本身，不自动拒绝旧 Worker。回收后旧 Worker 继续执行时，需要额外的 fencing/条件完成机制保护 DB 写入、终态和 ACK。仅新增唯一结果键也不能替代会话级顺序控制。

### 为什么不能取到就 ACK？

取到后 ACK，随后进程崩溃，队列就认为这次投递已完成；除非有独立、可靠的任务扫描恢复源，否则任务不再被正常回收。

### 为什么不能等前端收到后再 ACK？

HTTP/SSE 写成功不证明浏览器实际收到，更不证明页面已处理。若用户断网就拒绝 ACK，会反复回收已经成功的模型任务。消费完成必须与浏览器生命周期解耦。

### 失败任务何时 ACK？

- 安全的临时失败: 保留 pending，或先可靠移交到持久的延迟重试机制，再 ACK 原 entry；不能先 ACK 再尽力安排重试。
- 明确的永久失败: 先保存可查询的失败状态及必要诊断，再 ACK，不让毒消息无限占队。
- 上游结果不确定: 先记录可恢复的 uncertain 状态，交给查询/人工流程；未确认这种移交可靠前不能 ACK 后只留内存标记。
- `XACK` 不是删除 Stream 内容。裁剪/删除、幂等终态保留期和 pending 保护需要独立策略，不能激进裁剪未处理消息。

## 6. 不能忽略的崩溃窗口

| 窗口 | 只保留当前内存设计的结果 | 所需机制，具体选型待定 |
| --- | --- | --- |
| 已写用户消息，尚未入 Redis | 有聊天消息，无可执行任务 | SQL 可补投记录，或改 Redis-first，或明确接受此损失 |
| 已领取，尚未调用上游 | 内存状态丢失 | pending 回收、共享幂等/执行归属、完整任务描述 |
| 上游已执行，结果还未写 SQL | 重跑可能重复计费 | 上游幂等/查询能力，或 uncertain 策略；本地唯一键不能消除上游重复调用 |
| 最终 SQL 已提交，Redis 尚未 ACK | entry 会被再次看到，内存无法知道以前成功过 | 持久 request 关联/完成凭证；重投先检查，跳过模型与重复插入 |
| Redis 终态丢失，但 SQL 回复仍存在 | 无关联字段时难以判断对应哪个请求 | SQL 轻量完成凭证或其他可靠恢复依据；不能仅按正文匹配 |
| API/Worker 换实例 | 本机会话历史和幂等 map 不共享 | 内存降为缓存；共享协调与权威历史/版本检查 |

## 7. “不用完整请求表”的两条候选路线

### 路线 R: Redis-first，SQL 只保存业务结果及轻量凭证

API 通过 Redis 原子操作受理任务、建立共享幂等映射，再返回任务标识；Worker 按请求标识幂等写入 user/assistant 消息。SQL 可只增加关联字段与唯一约束，不必建立完整 `chat_requests` 状态机。

代价: Redis 成为关键业务存储，不再只是缓存；队列描述必须足够恢复，配额、终态、过期和对账要有明确语义。用户消息落库时机/SSE ack 含义需要改变，不能声称“完全沿用现有流程”。若确认入队后 Redis 灾难丢失，尚未进 SQL 的任务没有 SQL 依据可以恢复，必须接受相应 RPO 或加强持久保障。

### 路线 O: 保持 SQL-first，增加最小可补投记录

受理时在同一 SQL 事务里写用户消息和可恢复事件，后台发送 Redis。可以是小型 outbox 表，也可以是消息表上语义明确、可扫描的投递记录；但必须保存恢复所需的 Provider/model/附件快照等数据，不能凭一段聊天文本重建请求。

代价: 仍有少量 SQL schema 和事务改造，但不必做完整请求状态表。发送重试、结果唯一关联和 Redis ACK 仍需设计。

### 无法同时免费满足的组合

“SQL 完全不改 + 幂等/执行状态仍只在内存 + Redis 只负责转发 + 崩溃恢复与跨实例强幂等”不能同时成立。

可以裁掉完整请求表，不能裁掉**受理事实、完成凭证和故障恢复语义**。它们放在 Redis、SQL 或两者组合是可选项；凭空不存不是同等级替代方案。

## 8. 决策状态与下一步

用户已接受 `round-001.md` 的三项推荐: 已受理任务需有恢复依据、允许最小 SQL 改造、上游结果不确定时不自动重跑。未因此把 ACK 示例或物理 schema 视为已批准。

基于这些原则，用户在第 2 轮已接受路线 O，即 SQL-first + 轻量 Outbox + Redis Streams pull，详见 `adr-002-lightweight-outbox-and-durable-evidence.md`。Redis-first 仅保留为比较背景，不是当前实现方向；若以后重新选择，需重新评审恢复要求。

源码事实为静态核对；本轮没有连接运行中的 Redis、执行灾难测试或测量 SQL 性能。
