# 文档更新“远端已提交即成功”与本地历史对账 - 独立 Review

- [x] CP-1: PATCH 200 后本地历史失败仍成功且持久提交身份
  - **Type**: `rule`
  - **Covers**: AC-1
  - **Evidence**: paperless.go L1291-L1308 提交后仅经 `persistCommittedUpdate`（不返回 error）；commit_store.go 重试 3 次后入进程内队列；`TestUpdateDocuments_HistoryFailureAfterPatchStaysCommittedAndRetriesDedupe` 断言 err=nil、HistoryStatus=pending、commit 行含文档/字段/前后值、PATCH=1。独立审查以 `-race` 复跑 PASS。

- [x] CP-2: 同 commit_id 重试与重启对账零二次 PATCH、不重复历史
  - **Type**: `rule`
  - **Covers**: AC-2, AC-3
  - **Evidence**: 快速路径（paperless.go L701-L727）在字段装配前 continue，二次标签 PATCH 亦不可达；`(commit_id,mod_field)` 唯一索引 + 插入前查行；`ReconcilePendingCommits` 无任何 paperless client 依赖，启动即跑+30s ticker+ctx 取消（commit_store.go、main.go:281）。`TestUpdateDocuments_FastPathSkipsRemoteForKnownCommit`、`TestCompleteCommitIfPossible_BackfillsMissingRowsIdempotently`、`TestStartCommitReconciler_StartupPassAndShutdown`、队列故障恢复测试均 PASS。

- [x] CP-3: 提交前拒绝与无法确认错误保持旧语义
  - **Type**: `rule`
  - **Covers**: AC-7
  - **Evidence**: 400 strip-and-retry/非200/`client.Do` 错误/重试耗尽/预飞行失败均在 commit 持久化之前 `return nil, err`（paperless.go L630-L671、L988-L991、L1112-L1161），不产生成功 commit；既有 partial/400/网络测试断言语义不变，`TestUpdateDocumentsHandlerHardFailureStays500` PASS。

- [x] CP-4: 自动路径远端成功后不失败化、不重试、不重复消耗 provider
  - **Type**: `rule`
  - **Covers**: AC-6
  - **Evidence**: 恢复调用仅存在于 background.go 四处 `if err != nil` 分支（L339-L360、L472-L489、L528-L544、L665-L685）；成功（含 history pending）返回 nil 故不触发；工作流 OCR 成功后继续元数据生成。`TestProcessAutoOcrTagDocuments` 三场景断言 updateDocsCalls==1，-race PASS。

- [x] CP-5: 旧历史/Undo/正常成功路径兼容
  - **Type**: `rule`
  - **Covers**: AC-8
  - **Evidence**: 多 NULL commit_id 旧行共存测试 PASS；旧 `%v` 标签格式解析/拒绝逻辑未触碰；Undo 仍在 PATCH 后 SetModificationUndone（app_http_handlers.go undo handler 仅签名适配）；历史字段 prev/new（含 marshalTagsForHistory、appliedTagNames、custom_fields）经审查与 HEAD 逐行比对等价；TestUpdateDocuments_HistoryStoresHumanReadableNames 等 PASS。

- [x] CP-6: HTTP 契约与 Review/Playground 三态收敛及提示
  - **Type**: `rule`
  - **Covers**: AC-4, AC-5
  - **Evidence**: 成功 200 返回 commits[]（document_id/commit_id/already_committed/history_status），partial 结构保留并附 history_status；handler 三态+500 测试 PASS。前端任何 200 都 markApplied/onApplied 收敛；pending/recovered 使用 role=status 中性信息条（非红色 alert）；Playground 以 docId:text 为键，正文变更新 id，明确 4xx/5xx 清 id、网络错误保留。tsc 通过，lint 零新增问题，新 bundle 含三态文案（启动冒烟 8080 200）。

- [x] CP-7: 提交身份与持久化结构质量
  - **Type**: `rubric`
  - **Covers**: AC-9
  - **Scale**: 1-5
  - **Anchors**: 1 = 破坏旧数据/无幂等约束；3 = 可用但状态含糊；5 = 纯增量迁移、NULL 唯一约束正确、状态机自解释
  - **Pass Threshold**: >= 4
  - **Evidence**: 独立审查评分 **5**。AutoMigrate 纯增量（新表+可空指针列+复合唯一索引），旧行全 NULL 实测不冲突，状态常量/三时间戳/Attempts 语义清晰。

- [x] CP-8: 调用方收敛与信息表达质量
  - **Type**: `rubric`
  - **Covers**: AC-10
  - **Scale**: 1-5
  - **Anchors**: 1 = 误报失败或静默成功；3 = 行为对但提示含糊；5 = 三态在两条手动链路清晰且日志可辨识
  - **Pass Threshold**: >= 4
  - **Evidence**: 独立审查评分 **4**（自动路径对“成功但 pending”仅通用成功日志，无专门成功提示）。Review/Playground 三态文案明确；快速路径有 info 日志、队列写入有 error 日志。

## Advisory findings（不阻塞，来自 R1）

1. (medium) Review 端未按 payload 指纹刷新 commit id（字段排除集在歧义失败后变化时）。**已在 R1 后自愿修复**：SuggestionsReview.tsx 改为按 payload signature 绑定 id（与 Playground 的 text-key 等价），并重新 lint（仍 21 个基线问题，零新增）与 npm build 通过、dist 已重部署。
2. (medium) 测试缺口：快速路径 PartialUpdateError 重建、二次标签 PATCH 失败、多文档批次 outcomes。建议后续补。
3. (low) `withLocalRetries` 对确定性唯一冲突也重试 3 次；可按 gorm.ErrDuplicatedKey 短路。
4. (low) ChangesJSON 序列化序非确定（历史插入序确定，不影响正确性）。
5. (low) startup 对账同步执行（SQLite 本地，影响很小；spec 本要求启动先跑）。
6. (low) 补齐历史行 DateChanged 取补写时刻而非 commit.AppliedAt（同提交多行时间戳可能不一致）。
7. (low) 已知 commit 快速路径仍做预飞行 GET；paperless 恰不可达时重试会返回硬错误（FR-3 只要求零 PATCH，符合）。

环境披露（非本次改动问题）：`TestGetOcrPageResultsDefaultsToLatestRun` 在该 Windows 机约 50% flaky（ocr_runs.go `ORDER BY updated_at DESC` 缺次级排序键，代码本次未触碰）；既有 Windows 不兼容用例 3 个（onMount 反斜杠死循环、路径分隔符、POSIX chmod）与 ocr docling 错误文案差异不计入。

## Review History

### Review R1
- **Result**: `pass`
- **Evidence**: 全新上下文独立审查（只读），逐条核验 AC-1..AC-10；实际执行 `go test -race` 目标集、根包全量（按约定 -skip 3 个既有 Windows 用例）、`go vet`、`gofmt -l`、`npm run lint`（21 问题=基线）、字段历史循环与 HEAD 逐行比对、5 生产调用点+4 mock 适配核对；无 actionable 发现。
- **Checkpoint Results**: CP-1..CP-6 (`rule`): `pass`；CP-7 (`rubric`): `pass`, 5/5；CP-8 (`rubric`): `pass`, 4/5。
- **Post-R1**: 自愿修复 advisory #1（payload 指纹），经 lint/build/dist 冒烟复验；其余 advisory 记录留待后续，不影响验收。
