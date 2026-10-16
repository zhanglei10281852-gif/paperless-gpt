# 文档更新“远端已提交即成功”与本地历史对账 - 产品需求文档

## Overview
- **Summary**: 将 paperless-gpt 向 paperless-ngx 的文档 PATCH 成功（HTTP 200）确立为唯一提交点：远端接受后，本地修改历史（modification history）写入只允许成为“可恢复的待对账”状态，不再把普通错误返回给调用者；每次提交持久化可识别文档、实际落地字段及其前后值的提交身份（commit ledger），重试/重启只补齐缺失历史，不重复 PATCH、不产生重复历史；Review 与 OCR Playground 明确提示“已应用/历史补写中/已恢复”，自动标签、自动 OCR、工作流 OCR 在远端成功后不再执行失败恢复、重试计数或再次消耗 provider。
- **Purpose**: 修复“paperless-ngx 已显示新标题/标签/正文，但页面提示未写入；自动化路径把已更新文档当失败处理（加失败标签、重试、再次调用付费 provider）”的问题。
- **Target Users**: 在 Review 页面应用建议的用户、在 OCR Playground 应用正文的用户，以及依赖 Auto-Tag / 自动 OCR / 工作流 OCR 的无人值守用户。

## Goals
- 远端 PATCH 被接受（200）后，无论本地数据库状态如何，该次更新对所有调用方都是已提交的成功结果。
- 每次提交持久化一条 commit 记录：提交 ID、文档 ID、实际落地字段列表及每个字段的前值/新值、部分丢弃字段、历史对账状态。
- 本地历史写入部分失败、客户端用同一提交 ID 重试、或服务重启后：只插入缺失的历史记录；不再向 paperless-ngx 发送同一修改，不生成重复历史。
- Review 与 OCR Playground 在 200 后始终把文档收敛出待处理状态，并按“已应用且历史完整 / 已应用但历史仍在补写 / 此前已提交、历史已恢复”三种情况给出明确提示。
- Auto-Tag、自动 OCR、工作流 OCR 在远端成功后不触发失败标签恢复、不增加失败计数/重试、不重新调用 LLM/OCR provider。
- paperless-ngx 在提交前拒绝（400 等）或网络调用无法确认成功时，完整沿用现有失败、字段剔除重试（PartialUpdateError）与恢复逻辑。
- 现有 History 展示与 Undo、旧格式历史数据、无本地故障的手动与后台流程行为保持兼容。

## Non-Goals
- 不为“网络结果未知（超时/连接错误，无法确认 paperless-ngx 是否接受）”提供 exactly-once 语义：按需求明确沿用现有失败处理。
- 不重新设计 History/Undo 的数据格式与界面，不改动 OCR provider 调用、PDF 上传与建议生成流程本身。
- 不做多实例/跨主机对账；本地状态仍以单实例 SQLite 为准。
- 不新增用户可配置项（对账周期使用内置常量）。

## Background & Context
- 核心函数 [UpdateDocuments](file:///f:/swe/098861/project-03/paperless.go#L615-L1242)：先向 `api/documents/<id>/` 发 PATCH（含 400 字段剔除重试），成功后在 L1189-L1235 逐字段调用 `InsertModification`；**任一条历史插入失败就在 L1232-L1234 返回 error**，尽管远端修改已经落地。标签清空场景的第二次 PATCH（L1117-L1187）历史失败仅告警，两条路径行为不一致。
- 手动入口 [updateDocumentsHandler](file:///f:/swe/098861/project-03/app_http_handlers.go#L301-L333) 因此返回 500；前端 [SuggestionsReview.applyDocuments](file:///f:/swe/098861/project-03/web-app/src/components/SuggestionsReview.tsx#L156-L207) 与 [RunResults.handleApply](file:///f:/swe/098861/project-03/web-app/src/components/ocr/RunResults.tsx#L165-L180) 随即提示“写入失败”，文档保留在待处理状态。
- 自动入口把同一错误当作更新失败：[processTagDocuments](file:///f:/swe/098861/project-03/background.go#L339-L357)、[processAutoOcrTagDocuments](file:///f:/swe/098861/project-03/background.go#L665-L681)、[runWorkflowDocumentOCR](file:///f:/swe/098861/project-03/background.go#L472-L482) 调用 `recoverFromFailedUpdate`/`markProcessingFailed`（移除触发标签并加 FAIL_TAG）、计入 errs/退避；工作流 OCR 的内容 PATCH 不移除工作流触发标签，下一周期会重新 OCR、再次产生 provider 费用。
- 历史表 [ModificationHistory](file:///f:/swe/098861/project-03/local_db.go#L13-L23) 无请求标识、无幂等约束；`InsertModification` 无 request id / ON CONFLICT。重试同一请求会再次 PATCH 并产生重复历史行。
- DB 为本地 SQLite（`db/modification_history.db`），GORM AutoMigrate 初始化；故障形态以瞬时不可写为主（锁、磁盘、权限），重启后可恢复。

## Functional Requirements

### FR-1：提交账本（commit ledger）持久化
- 新增一张提交表（由 AutoMigrate 创建），每条记录至少包含：`commit_id`（UUID，主键）、`document_id`、`is_undo`、状态（远端已确认）、`history_status`（`pending`/`complete`）、落地字段清单 JSON（每项含 field/previous_value/new_value）、部分丢弃字段 JSON、创建/应用/更新时间、对账尝试次数。
- `ModificationHistory` 增加可空 `commit_id`（旧数据为 NULL），并以 `(commit_id, mod_field)` 唯一索引保证同一提交同字段不重复；NULL 不受唯一约束影响（SQLite 多 NULL 互不冲突）。

### FR-2：PATCH 200 后本地写入不再返回错误
- `UpdateDocuments` 在 PATCH 确认成功后：先持久化 commit 记录（含落地字段与前后值），再写入历史行；历史写入部分/全部失败只把 `history_status` 置为 `pending` 并排队对账，函数整体仍返回成功（携带每文档的历史状态结果），绝不因本地写入失败返回普通 error。
- 标签清空场景的第二次 PATCH 所产生的 tags 历史，并入同一提交记录与同一套对账逻辑，不再旁路直写。
- commit 记录自身写入瞬时失败时做有限次重试；仍失败则放入进程内待写队列由对账循环重试（覆盖进程生命周期），并记录 error 级日志；远端结果依旧按成功返回。

### FR-3：提交 ID 幂等重试
- `DocumentSuggestion` 增加可选 `commit_id`；Review 与 Playground 的每次“提交动作”生成 UUID，并在对同一未决提交的重试（含网络错误后的再次点击/自动重试）中复用；服务器在未携带时自行生成。
- 服务器收到已存在且远端已确认的 `commit_id` 时：不再发送任何 PATCH，只对该提交执行历史对账（补齐缺失行），返回成功并标注 `already_committed`；若该提交原本是部分字段丢弃，幂等地重现 PartialUpdateError 信息（失败标签追加本身具备幂等检查）。
- 仅当服务器明确拒绝（收到 4xx/5xx 响应）后客户端才换用新的 commit_id；网络不可达/超时等无法确认结果时保留原 commit_id。

### FR-4：对账循环（启动 + 定时 + 进程内队列）
- 服务启动后先执行一次对账，并在后台按固定间隔运行：加载 `history_status=pending` 的 commit，对照落地字段清单插入缺失历史行（以 `(commit_id, mod_field)` 防重），补齐后置为 `complete`；同时重试进程内待写队列中的 commit 记录。
- 对账只写本地数据库，绝不向 paperless-ngx 发请求。

### FR-5：HTTP 响应契约
- `PATCH /api/update-documents` 在远端确认成功时始终返回 200，响应体包含每文档结果：`document_id`、`commit_id`、`already_committed`、`history_status`（`complete`/`pending`）；部分丢弃时保留现有顶层 `partial` 结构并附带历史状态。
- 远端确认前的失败仍按现状返回 4xx/5xx 与错误体。

### FR-6：Review 页面提示与收敛
- 200（含 `history_status=pending` 或 `already_committed=true`）后文档立即标记为 applied、离开待处理列表与计数；以信息提示（非错误红条）说明：已应用且历史完整 / 已应用但本地历史仍在补写 / 此前提交已确认且历史已恢复。
- 仅在明确的 4xx/5xx 时显示失败并允许重新提交（新 commit_id）；网络错误保留提交身份以便重试去重。

### FR-7：OCR Playground 提示与收敛
- Apply 成功后按 FR-6 三种状态显示 toast（含现有 “Review in History” 动作）；失败提示维持现状，且正文未被确认写入时不改变成功状态。
- 若用户在未确认成功后修改了正文再提交，使用新的 commit_id（避免幂等短路应用旧文本）。

### FR-8：自动化路径远端成功后不得失败化
- Auto-Tag、自动 OCR、工作流 OCR（含“文档已有 OCR 完成标签仅移除触发标签”“部分字段丢弃后加失败标签”等现有分支）在 PATCH 200 后：不调用 `recoverFromFailedUpdate`/`markProcessingFailed`、不追加 FAIL_TAG、不计入失败计数/退避、不重新触发或重复执行 LLM/OCR provider 调用；工作流 OCR 成功后继续元数据生成流程。

### FR-9：既有失败语义保持
- PATCH 返回 400 的字段剔除重试与 `PartialUpdateError`、非 400/非 200 硬错误、`client.Do` 网络错误（无法确认成功）的行为与调用方恢复逻辑完全保持现状。
- 预飞行失败（取标签/往来用户/文档类型/自定义字段失败、JSON 序列化失败等，PATCH 尚未发出）仍直接返回错误。

### FR-10：兼容性
- 旧 `ModificationHistory` 数据（NULL commit_id、含旧版 `%v` 标签格式）在 History 列表与 Undo 中行为不变。
- Undo 流程（PATCH 后 `SetModificationUndone`）保持可用；Undo 产生的 PATCH 同样留下 commit 记录但不改变 Undo 语义。
- 正常无本地故障的成功路径产生的历史行字段、值、条数与现状一致。

## Non-Functional Requirements
- **NFR-1 可靠性**：所有新增本地写路径必须幂等；对账可重复执行无副作用；不引入对 paperless-ngx 的额外请求（幂等重试路径为零请求）。
- **NFR-2 可测性**：后端新增单元测试覆盖：提交记录写入、历史半写后补齐、同 commit_id 重试不产生第二次 PATCH/重复历史、重启后启动对账、PATCH 200 后历史库故障返回成功、自动化路径不触发恢复；保持现有 Go 测试通过。
- **NFR-3 质量门禁**：`go test ./...`、`gofmt -l .` 无输出、`cd web-app && npm run lint` 通过；前端改动后执行 `npm run build` 并 `cp -r dist ../`。
- **NFR-4 性能**：每次更新仅增加一次本地 SQLite 写（commit 行）；对账按小批量、固定间隔运行，不影响主请求延迟。

## Constraints
- **Technical**: Go + Gin + GORM（gorm v1.31.2，sqlite v1.6.0），UUID 使用已有 `github.com/google/uuid`；前端 React + TypeScript + axios；数据库为本地 SQLite 单文件；CGO 构建要求不变。
- **Business**: 不改变对 paperless-ngx 的 API 契约；不引入新外部依赖与配置项。
- **Dependencies**: 所有文档写回路径已统一经过 `UpdateDocuments`（手动 Review、Playground、Undo、Auto-Tag、自动 OCR、工作流 OCR、失败标签恢复）。

## Assumptions
- paperless-ngx 对同一文档重复 PATCH 相同字段值本身近似幂等，但本方案在已知 commit_id 的情况下完全不重复发送，以避免触发标签/完成标签被二次改写。
- 本地 SQLite 故障多为瞬时；进程内待写队列只承诺进程生命周期内不丢，极端情况（commit 行写入期间掉电且重启）允许丢失对账线索，此时按 FR-9 的“无法确认”既有语义处理。
- 前端按文档逐篇发送（现状一次请求一个文档）；批量接口的每文档结果以数组返回以保持正确。

## Acceptance Criteria

### AC-1：PATCH 200 后历史写入失败仍返回成功并留下提交身份
- **Type**: `rule`
- **Given**: paperless-ngx 对文档 PATCH 返回 200，而本地 SQLite 在写 `ModificationHistory` 时失败（如注入故障）
- **When**: 调用 `UpdateDocuments`
- **Then**: 返回的结果无错误，结果中标注该文档 `history_status=pending`；commit 表存在对应记录，含 document_id、全部落地字段及各项 previous/new 值
- **Pass Condition**: 单元测试中模拟历史写故障，断言返回成功、commit 行内容完整、且 mock paperless 只收到一次 PATCH
- **Evidence**: 新增 Go 测试输出 + 代码位置

### AC-2：历史半写后同 commit_id 重试只补齐缺失历史且不再 PATCH
- **Type**: `rule`
- **Given**: 一次提交的 3 个落地字段只写入了 2 条历史（commit 行为 pending）
- **When**: 客户端用相同 `commit_id` 再次调用更新接口
- **Then**: 不向 paperless-ngx 发送 PATCH；第 3 条历史被补齐，commit 置为 complete，`(commit_id, field)` 无重复行，响应 `already_committed=true`
- **Pass Condition**: 单元测试断言 PATCH 计数不增加、历史恰好 3 条且字段齐全、第二次返回已提交标记
- **Evidence**: 新增 Go 测试输出

### AC-3：服务重启后待对账提交被自动补齐
- **Type**: `rule`
- **Given**: 数据库存在 `history_status=pending`、历史行缺失的 commit（模拟服务中断）
- **When**: 启动时对账与定时对账运行
- **Then**: 缺失历史按落地字段清单补齐，commit 转为 complete；全过程无 paperless-ngx 请求
- **Pass Condition**: 对账函数测试 + 启动调用链检查（main.go）
- **Evidence**: Go 测试输出 + main.go 接线证据

### AC-4：HTTP 200 响应携带每文档历史状态，前端 Review 正确收敛并提示
- **Type**: `rule`
- **Given**: Review 页面应用一个文档建议
- **When**: 后端返回 200 且 `history_status` 为 complete/pending 或 `already_committed=true`
- **Then**: 文档立即离开 pending（标记 applied、计数收敛），页面分别展示“已应用”“历史仍在补写”“历史已恢复”信息提示而非错误；不出现红色失败条
- **Pass Condition**: 前端代码走查 + lint/tsc 通过；构建后页面可实际操作验证三种文案分支
- **Evidence**: SuggestionsReview.tsx 代码 + 构建/运行验证

### AC-5：OCR Playground 按状态提示并在正文变更时刷新提交身份
- **Type**: `rule`
- **Given**: Playground 应用 OCR 正文
- **When**: 返回 complete/pending/recovered；或网络失败后用户修改正文再次提交
- **Then**: toast 文案与状态对应；修改正文后的重试携带新 commit_id；未确认成功时不显示成功 toast
- **Pass Condition**: RunResults.tsx/api.ts 代码走查 + lint/tsc 通过
- **Evidence**: 前端代码 + 构建验证

### AC-6：自动化路径在远端成功后不失败化、不重试、不重复消耗 provider
- **Type**: `rule`
- **Given**: Auto-Tag / 自动 OCR / 工作流 OCR 的更新 PATCH 已 200，但随后本地历史写失败
- **When**: 三个处理路径继续执行
- **Then**: 不调用失败标签恢复、不加 FAIL_TAG、不计失败计数/退避；工作流 OCR 继续元数据生成；下一周期不因该事件重新调用 LLM/OCR
- **Pass Condition**: 代码走查确认成功路径无恢复调用 + Go 测试断言 UpdateDocuments 成功后无恢复 PATCH（FAIL_TAG 不出现）
- **Evidence**: background.go 走查 + 测试输出

### AC-7：提交前拒绝与无法确认的网络错误沿用旧逻辑
- **Type**: `rule`
- **Given**: 场景 a) PATCH 返回 400 且字段可剔除；b) PATCH 非 200/非 400；c) `client.Do` 网络错误；d) 预飞行取数失败
- **When**: 调用更新
- **Then**: a) 仍走 strip-and-retry 并返回 PartialUpdateError（失败标签逻辑不变）；b)/c)/d) 仍返回硬错误，后台路径仍执行现有恢复；这些场景不产生“成功但历史 pending”的 commit 记录
- **Pass Condition**: 既有相关 Go 测试全部通过不改动断言语义
- **Evidence**: `go test ./...` 输出

### AC-8：旧历史、Undo 与正常成功路径兼容
- **Type**: `rule`
- **Given**: 数据库含旧 commit_id 为 NULL 的历史（含旧标签文本格式）；以及一次完全正常无故障的成功更新
- **Then**: 历史列表与 Undo 行为不变（旧格式拒绝逻辑保留）；正常成功产生字段/值/条数一致的历史行且 commit 为 complete；Undo PATCH 后仍能标记 undone
- **Pass Condition**: paperless_history_test.go 等既有测试通过 + Undo 手工/测试验证
- **Evidence**: Go 测试输出

### AC-9：提交身份与持久化结构质量
- **Type**: `rubric`
- **Dimension**: 数据模型与迁移的稳妥性（可空演进、幂等约束、时间戳与状态机清晰、无破坏性迁移）
- **Scale**: 1-5
- **Anchors**: 1 = 破坏旧数据或无幂等约束；3 = 功能可用但缺索引/状态含糊；5 = 纯增量迁移、唯一约束正确覆盖 NULL、状态与字段语义自解释
- **Pass Threshold**: >= 4
- **Evidence**: local_db.go / 新 commit store 文件与迁移代码走查

### AC-10：调用方收敛与信息表达质量
- **Type**: `rubric`
- **Dimension**: 三条手动/自动链路对“已提交 vs 失败”的表达一致性与用户可理解性
- **Scale**: 1-5
- **Anchors**: 1 = 仍有误报失败或静默成功；3 = 行为正确但提示含糊；5 = 三类状态在 Review/Playground 均有清晰、非误导性表达，自动化日志同样可辨识
- **Pass Threshold**: >= 4
- **Evidence**: 前端文案、后台日志走查与运行验证

## Open Questions
- 无阻塞性开放问题；以下为实现期采用的默认决策，可在评审时调整：
  - [ ] 对账间隔默认 30s，启动时立即执行一次（小批量，每批限数）。
  - [ ] commit 行本地写入瞬时失败时同步重试 3 次（短退避），随后进入进程内队列。
  - [ ] 响应体使用 `commits[]`（每文档一项）+ 保留顶层 `partial`。
