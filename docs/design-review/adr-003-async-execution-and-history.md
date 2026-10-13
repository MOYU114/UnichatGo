# ADR-003: 后台独立执行与 MySQL 历史恢复

- 日期: 2026-10-11。
- 状态: **Accepted，异步生命周期、历史恢复与错误展示边界均已接受**。
- 确认依据: 用户要求前端发送后由后端自行完成，任务不依赖原请求连接，后续通过 MySQL 恢复历史；第 3 轮回复“对的，这么理解即可”，接受内部最终失败/不确定状态也如实展示。
- 范围: 明确语义与实现约束；具体接口路径、在线流式体验及取消 UI 不在本次自动定版。

## 1. 已确认语义

1. 前端发起生成，后端可靠受理后独立执行。页面关闭、刷新、网络断开和 Gateway 下线均不取消已受理任务，也不触发重复模型调用。
2. Worker/执行服务负责结果落库与消费确认。原 Handler 不负责等待模型完成后再保存回复。
3. 最终回复和必要业务状态由 MySQL 保存；用户回来时通过鉴权后的历史 API 读取，不需要找回原 Worker、连接或浏览器内存。
4. 如保留 SSE，它只是可选的观察通道。无订阅者、慢客户端或写事件失败不能使任务失败；不为最终历史恢复强制实现逐 token 续传。
5. 浏览器是否收到 done 不参与 SQL 提交或 Redis ACK 判定。断连是连接状态，不是模型任务终态。

用户的“前端发送后”需要以**后端已可靠受理**为边界: 浏览器调用发送函数不证明请求抵达或 SQL 已提交。受理应答丢失时用同一 client ID 查询/重发核对，不能先生成新 key。

## 2. 推荐接口形态，具体路径尚未定版

```text
POST 提交
  -> MySQL 原子受理
  <- 202 + request_id + user_message_id

后台: Outbox -> Redis -> Worker -> MySQL 最终提交 -> XACK

GET 会话历史
  <- 已提交 user/assistant 消息
  <- 必要的 queued/running/completed/failed/uncertain 状态

可选 GET 进度订阅
  <- 仅观察，不启动/取消任务
```

历史中如果只有 user 消息，没有 assistant 回复，不能据此判断是等待中、上游失败、内部失败还是结果未知。建议由后端将最小执行凭证中的状态投影到历史响应，不必因此新增第二套大型状态存储，也不要求前端持续轮询。

## 3. 生命周期实现约束

- 受理阶段使用 HTTP Context 处理身份校验、事务及应答；可靠提交后，Worker 从持久任务数据建立服务管理的执行 Context，不继承原 HTTP 请求的取消信号。
- 独立执行不等于无限期 `context.Background()`：仍需任务 deadline、停机处理、lease/fence 和资源上限，不能生成无管理的后台 goroutine 后即宣称可靠异步。
- 消费者在后台提交 user/assistant 关联、最终状态和会话推进；SSE 发送成功不能替代事务提交。
- 结果在内存中且 SQL 暂时不可用时，可在有界策略内重试保存该结果，不重新调用模型。若进程在保存前崩溃且没有可靠结果副本，不能假设一定能从历史恢复，应进入既定的不确定处理。
- 停机/队列恢复只能重试确认安全的阶段。上游可能已执行时沿用已接受的 uncertain 策略，不因前端不在线就放宽重复计费保护。

## 4. 已确认的错误展示边界

**已确认:** 前端断连或没有观察者不应把后台生成标记失败；内部短暂故障优先由后台恢复，不把每次重投、续租或暂时写库失败都弹窗给用户。

**第 3 轮已确认:** 供应商失败、内部故障耗尽恢复预算或结果不确定时，向用户展示真实且可区分的状态。不能隐瞒内部故障、伪装成功或无限显示“生成中”。这不引入浏览器与后台执行的依赖。

| 情况 | 已确认的行为原则 |
| --- | --- |
| 浏览器断网，任务正常执行 | 不改任务状态；用户回来读取 MySQL 历史 |
| Redis 暂时不可用，但 SQL 已可靠受理 | 保留待处理状态，由 Outbox 在恢复后投递；是否超时按任务预算决定 |
| Provider 明确拒绝、认证失效或不可恢复错误 | 保存归因明确的失败状态，历史中提示，不假装成功 |
| Provider 中途断流，无法确认完整结果 | 区分正常 EOF 和异常终止；不把半句回复无条件保存为成功 |
| 已拿到回复，但最终写库持续失败 | 后台有界重试保存，不重复调用模型；无法完成时显示保存/系统异常或不确定状态 |
| Worker 崩溃且上游可能已执行 | 恢复后记录 uncertain，阻塞同会话后续，不把无 ACK 等同于未调用 |
| DB 整体不可用，无法查询历史/状态 | 返回服务暂不可用；不能伪造某条任务已失败或已完成 |

恢复状态本身也依赖可写存储；SQL 不可用时不声称已经持久标记了失败。恢复后再依据可验证事实对账，并在查询不可用期间如实返回服务错误。

## 5. 设计意图不等于现有实现已完成

当前代码已有本机 Worker/channel 和 SQL 历史，但这些机制本身不证明任务生命周期独立。现有 HTTP Context、Worker 等待及最终落库边界需要核对并改造；不将用户表达的原始设计意图误写成现有代码已经满足。

本轮只读复核结果:

| 已验证事实 | 源码定位 | 改造含义 |
| --- | --- | --- |
| Handler 从 HTTP Context 创建模型任务 Context，Manager 入队后仍等待结果 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:407`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/worker/manager.go:245` | goroutine 执行不等于任务生命周期脱离连接 |
| chunk 回调直接写原响应，最终 assistant 落库也使用原 HTTP Context | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:432`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:446` | 两条路径都需与任务执行解耦；不能保证现有版本断连后继续落库，也不能保证断连立即撤销上游计费 |
| 历史 API 只返回 session/messages，Message 无任务状态 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/api/handlers.go:342`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/models/message.go:15` | 可恢复已保存消息，但无法区分等待、失败及 uncertain |
| Provider 流读取遇到任何错误都退出循环，随后返回成功结果 | `/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/ai/service.go:146`、`/Users/moyu114/Documents/agent_unity/UnichatGo/backend/internal/service/ai/service.go:168` | 需区分正常 EOF 与异常中断，不能把当前误吞流错误的行为复制到新消费状态机 |

这些是静态代码事实，不是已复现的线上故障。成功完成应满足 Provider 正常结束及业务结果提交，而不是仅以“读取循环结束”作为判定。

本 ADR 不自动批准显式取消 UI、永久流事件存储或独立 SSE 微服务。先保证受理可靠、后台独立和历史可恢复。
