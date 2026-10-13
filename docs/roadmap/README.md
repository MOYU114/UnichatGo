# UnichatGo Roadmap

本目录记录改造阶段、依赖、交付物和验收进度。目录与文件使用英文名称，正文可以使用中文。

## 当前路线图

- [异步执行与水平扩容路线图](./async-execution-and-horizontal-scaling.md)

## 文档职责

- `docs/design-review/`: 为什么这样设计，哪些决策已接受，哪些细节仍需审阅。
- `docs/roadmap/`: 按什么顺序推进，每阶段交付什么，达到什么条件才算完成。
- 原始 Issue 001: 保留初始问题与源码调查背景；其中已被后续决策替代的方案不再作为实施依据。

## 维护规则

1. 设计依据以 [design-review](../design-review/README.md) 中最新已接受的决策为准；路线图不能自行将待决方案变成已批准方案。
2. 更新阶段状态时附带实际产物和验收证据；“代码写完”“文档写完”不等于“测试通过”或“可以上线”。
3. 所有阶段按依赖和发布门槛推进，暂不承诺未经评估的日期、工期、QPS 或并发容量。
4. 2026-10-12 用户已授权本地隔离实施与验证；不含生产部署、真实数据迁移、无预算付费调用或远端推送。阶段完成以检查点证据为准。

## 实施记录

- [2026-10-12 实施检查点](./implementation-checkpoint-2026-10-12.md)
- [回归修复与独立复审结果](../design-review/review-005-regression-fixes.md)
- [Async 运行说明](./async-operations.md)
- [隔离测试运行说明](./async-test-runbook.md)
- [旧附件迁移](./legacy-attachment-migration.md)
