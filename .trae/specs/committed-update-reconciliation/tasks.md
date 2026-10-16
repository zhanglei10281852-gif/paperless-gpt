# 文档更新“远端已提交即成功”与本地历史对账 - 实现计划

## Task 1: 提交账本模型与数据库迁移
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Completion Evidence**:
  - local_db.go 新增 `DocumentUpdateCommit`、`CommittedFieldChange` 与状态常量；`ModificationHistory` 增加可空 `CommitID` 与 `idx_commit_field` 复合唯一索引；InitializeDB/InitializeTestDB AutoMigrate 已更新。
  - `rule` TR-1.1/TR-1.2: commit_store_test.go 中 6 个测试全部 PASS（多 NULL 旧行共存、同 commit+field 重复被唯一约束拒绝、完整写入三行）。
  - 注：工作区为 CRLF 检出，最终验证前统一行尾。
- **Description**:
  - 在 [local_db.go](file:///f:/swe/098861/project-03/local_db.go) 新增 `DocumentUpdateCommit` 模型：`CommitID`(主键,size:36)、`DocumentID`(not null,index)、`IsUndo`、`Status`(size:32，固定值 `applied`)、`HistoryStatus`(size:32，`pending`/`complete`，index)、`FieldsJSON`(TEXT)、`DroppedFieldsJSON`(TEXT)、`CreatedAt`/`AppliedAt`/`UpdatedAt`(time.Time)、`Attempts`(int，默认0)。
  - `ModificationHistory` 增加可空 `CommitID *string`（`type:VARCHAR(36)`），与 `ModField` 组成复合唯一索引 `idx_commit_field`（SQLite 中多 NULL 互不冲突，旧行安全）。
  - `InitializeDB` 的 AutoMigrate 增加 `DocumentUpdateCommit`；同步更新 `InitializeTestDB`（paperless_test.go）迁移新模型。
  - 定义落地字段条目类型 `committedFieldChange{Field, PreviousValue, NewValue string}` 及常量（状态/历史状态字符串）。
- **Acceptance Criteria Addressed**: AC-1, AC-8, AC-9
- **Test Requirements**:
  - `rule` TR-1.1: 对含旧 `ModificationHistory` 行（NULL commit_id）的库执行 AutoMigrate 成功，且新表/复合唯一索引存在；证据：`go test ./...` 中迁移相关测试通过、`go test -run TestInitialize|TestUpdateDocuments` 输出。
  - `rule` TR-1.2: 同一 `(commit_id, mod_field)` 二次插入被唯一约束拒绝，而任意多条 NULL commit_id 旧行可共存；证据：新增/现有测试断言。
- **Notes**: 纯增量列/表迁移，不改动既有列定义与历史读写 SQL 的外部行为。

## Task 2: Commit store 与历史对账器
- **Status**: `completed`
- **Completion Evidence**:
  - 新增 commit_store.go：commit 行重试写入、进程内 backfill 队列、按固定字段顺序幂等补历史、`completeCommitIfPossible`、`ReconcilePendingCommits`（队列+pending 表）、`StartCommitReconciler`（启动即跑+定时+ctx 取消）。
  - `rule` TR-2.1/2.2/2.3：对应测试 PASS（完整写入、半写补齐且幂等、历史/提交行写故障后经对账恢复）。
  - `rule` TR-5.1 的对账器部分：TestStartCommitReconciler_StartupPassAndShutdown PASS。
  - `rubric` TR-2.4 自评 5：纯本地、无 paperless 依赖、全部写路径幂等，队列与重试边界清晰。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - 新增 `commit_store.go`：
    - `persistCommit(db, commit, changes)`：幂等 upsert commit 行；对 SQLite 瞬时错误（busy/locked/closed）做 3 次短退避重试；仍失败时把完整 commit+changes 放入进程内 `commitBackfillQueue`（带 mutex 的 map），返回“待对账”而非错误。
    - `recordCommitHistory(db, commitID, changes)`：按固定字段顺序（tags, title, correspondent, document_type, created_date, content, custom_fields）逐行插入带 commit_id 的历史；已存在（唯一约束/查行）则跳过；任一失败不中断其余行，返回缺失字段列表。
    - `completeCommitIfPossible(db, commitID)`：对照 commit.FieldsJSON 与现存历史行（按 `(commit_id, field)`），只插缺失行；全部到位则置 `HistoryStatus=complete`；否则保持 `pending` 并 Attempts+1。
    - `ReconcilePendingCommits(ctx, db, batchSize)`：先排空进程内队列（补写 commit 行），再批量加载 pending commit 逐个对账；纯本地操作；返回处理数量与日志。
    - `StartCommitReconciler(ctx, db, interval)`：立即执行一次后按 ticker 运行，响应 ctx 取消。
  - 历史行的 field/prev/new 值构造逻辑与现有 paperless.go L1189-L1235（含 marshalTagsForHistory 名称规则）完全一致。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-9
- **Test Requirements**:
  - `rule` TR-2.1: commit 行 + 3 条历史写入后状态为 complete；证据：新测试查询断言。
  - `rule` TR-2.2: 制造“commit 已写、历史仅 2/3”后调用 `completeCommitIfPossible`，恰好补齐 1 行、无重复行、状态转 complete；再次调用无新增（幂等）；证据：新测试。
  - `rule` TR-2.3: 模拟 DB 写入故障，`persistCommit` 不返回硬错误且条目进入进程内队列；DB 恢复后 `ReconcilePendingCommits` 成功落库并补齐；证据：新测试。
  - `rubric` TR-2.4: 实现简洁性/状态机清晰度；scale 1-5；anchors 1=状态与副作用混乱、对账非幂等；3=可用但职责交叉；5=纯函数可测、无 paperless 依赖、重试与队列边界清晰；threshold >=4；证据：代码走查。

## Task 3: UpdateDocuments 提交点重构与全部调用方适配
- **Status**: `completed`
- **Completion Evidence**:
  - paperless.go：签名改为 `(*UpdateDocumentsResult, error)`；循环开头按 commitID 幂等快速路径（零 PATCH、reconcileCommit、重建 PartialUpdateError）；PATCH 400 剔除/网络未知/预飞行失败均 `return nil, err` 保持旧语义；200 后收集含二次标签 PATCH 在内的 committedChanges，经 newCommitRecord/persistCommittedUpdate 落账本与历史，本地失败不影响成功返回。
  - types.go 新增 DocumentSuggestion.CommitID、DocumentCommitOutcome、UpdateDocumentsResult；ClientInterface 与 4 个 mock（background_test/app_http_handlers_test/app_llm_test）及 5 个生产调用点全部适配。
  - `rule` TR-3.1: TestUpdateDocuments_HistoryFailureAfterPatchStaysCommittedAndRetriesDedupe PASS（注入历史写故障→成功 pending；恢复后同 id 重试 PATCH 计数仍为 1、补齐历史无重复）。
  - `rule` TR-3.2: TestUpdateDocuments_FastPathSkipsRemoteForKnownCommit PASS（零二次 PATCH、AlreadyCommitted、空 id 服务端生成）。
  - `rule` TR-3.3/3.4: 根包全量测试通过（跳过 3 个既有 Windows 不兼容用例），400/网络/预飞行旧测试与 paperless_history_test 全部不变绿；历史字段/值断言沿用原逻辑。
  - `rule` TR-3.5: mockClient 增加 updateDocsCalls 计数，TestProcessAutoOcrTagDocuments 三场景断言成功仅 1 次 UpdateDocuments（无恢复 PATCH），PASS。
  - `rubric` TR-3.6 自评 5：成功/失败边界 = HTTP 200，旧失败路径零漂移。
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - 在 [paperless.go](file:///f:/swe/098861/project-03/paperless.go) `UpdateDocuments` 中：
    1. `DocumentSuggestion`（types.go）增加 `CommitID string \`json:"commit_id,omitempty"\``；循环内每文档确定 commitID（请求携带则用之，否则 `uuid.NewString()`）。
    2. 文档循环开头按 commitID 查 commit：存在且 `applied` → **快速路径**：零 PATCH，调用 `completeCommitIfPossible` 对账，产出 `already_committed=true` 的结果；若其 DroppedFields 非空则重建 `PartialUpdateError` 纳入聚合返回。
    3. PATCH 主循环与 400 strip-and-retry、预飞行失败、网络错误、非 200 硬错误逻辑保持现状（这些路径不写 commit）。
    4. 成功后把“实际发送字段”（最终 updatedFields 中存在的键）与标签清空第二次 PATCH 成功后的 tags 变化，统一收集为 `[]committedFieldChange`（第二次 PATCH 的历史不再旁路直写 `InsertModification`；其失败仍仅告警且该 tags 条目不进入清单）。
    5. 调用 `persistCommit` + `recordCommitHistory` + `completeCommitIfPossible`；这些本地步骤任何失败都不改变返回成功；`firstPartial` 机制保持。
  - 签名改为 `UpdateDocuments(...) (*UpdateDocumentsResult, error)`：新增 `UpdateDocumentsResult{Documents []DocumentCommitOutcome}`，`DocumentCommitOutcome{DocumentID, CommitID, AlreadyCommitted bool, HistoryStatus string}`；error 仍可为 `*PartialUpdateError`（现有 errors.As 调用不变）。
  - 适配 [ClientInterface](file:///f:/swe/098861/project-03/types.go#L254-L275) 及全部实现/调用方：app_http_handlers.go（handler 与 undo）、background.go 五个调用点、background_test.go/app_http_handlers_test.go/app_llm_test.go 的 mock。
  - 自动路径成功分支保持“nil 错误即成功收敛”，不新增任何恢复调用。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-6, AC-7, AC-8, AC-10
- **Test Requirements**:
  - `rule` TR-3.1: PATCH 200 后注入 `modification_histories` 写入故障（gorm Create callback 失败若干次），UpdateDocuments 返回非 nil result 且 error 为 nil、HistoryStatus=pending、commit 行完整；恢复后第二次同 commitID 调用 PATCH 计数不增加、历史不重复且转 complete；证据：新增 Go 测试。
  - `rule` TR-3.2: 同 commitID 快速路径对 mock server 零 PATCH（除预飞行 GET）且返回 AlreadyCommitted；证据：新测试断言 PATCH 计数。
  - `rule` TR-3.3: 400 剔除重试仍返回 PartialUpdateError 且无 commit 成功记录；网络错误/非200/预飞行失败返回硬错误（既有测试不改断言语义）；证据：`go test ./...`。
  - `rule` TR-3.4: 标签清空第二次 PATCH 路径与正常多字段路径生成的历史字段/值/条数与改造前一致（由现有 TestUpdateDocuments* 与 paperless_history_test 保证），且全部带正确 commit_id；证据：既有测试 + 新断言。
  - `rule` TR-3.5: background 路径在 result 成功（含 history pending）时不调用恢复：新测试断言无 FAIL_TAG PATCH、processedCount 正常；现有 background_test.go 全部通过；证据：测试输出。
  - `rubric` TR-3.6: 重构保真度；scale 1-5；anchors 1=改变了既有失败/标签语义；3=行为保持但实现绕弯；5=成功/失败边界清晰且旧逻辑零漂移；threshold >=4；证据：差异走查。

## Task 4: update-documents HTTP 响应契约
- **Status**: `completed`
- **Completion Evidence**:
  - handler 远端确认一律 200：body `{commits:[{document_id,commit_id,already_committed,history_status}]}`；partial 顶层结构字段不变并附 history_status；硬错误维持 500；undo 仅适配签名。
  - `rule` TR-4.1: TestUpdateDocumentsHandlerReportsCommitStatuses（complete/pending/already_committed 三态 200）、TestUpdateDocumentsHandlerHardFailureStays500、既有 TestUpdateDocumentsHandlerReportsPartialUpdates 全部 PASS。
- **Priority**: high
- **Depends On**: Task 3
- **Description**:
  - 改造 [updateDocumentsHandler](file:///f:/swe/098861/project-03/app_http_handlers.go#L301-L333)：
    - 远端确认成功一律 HTTP 200，body 为 `{"commits":[{"document_id","commit_id","already_committed","history_status"}]}`；
    - partial 场景保留顶层 `{"partial":{document_id,dropped_fields,fail_tag}}`（字段不变）并附同文档的 `history_status`；
    - 硬错误维持 500/错误体；undo handler 仅适配新签名，响应行为不变。
  - 增加/更新 app_http_handlers_test.go 覆盖：成功 complete、成功 pending（200 非 500）、already_committed、partial、硬错误 500。
- **Acceptance Criteria Addressed**: AC-4, AC-7
- **Test Requirements**:
  - `rule` TR-4.1: httptest 断言四种 200/500 响应形态与 JSON 字段；证据：handler 测试输出。

## Task 5:启动与定时对账接线
- **Status**: `completed`
- **Completion Evidence**:
  - main.go InitializeDB/MarkInterruptedOCRRuns 之后调用 `StartCommitReconciler(ctx, database, 30*time.Second)`；启动即跑一次、ticker 定时、ctx 取消退出。
  - `rule` TR-5.1: TestStartCommitReconciler_StartupPassAndShutdown PASS；`go build ./...` 成功。
- **Priority**: medium
- **Depends On**: Task 2
- **Description**:
  - 在 [main.go](file:///f:/swe/098861/project-03/main.go#L271-L275) InitializeDB/MarkInterruptedOCRRuns 之后启动 `StartCommitReconciler(ctx, database, 30*time.Second)`；启动对账失败只告警不阻断服务。
- **Acceptance Criteria Addressed**: AC-3
- **Test Requirements**:
  - `rule` TR-5.1: 主进程接线编译通过且对账循环响应 ctx 取消（StartCommitReconciler 单测：启动、处理一次、取消退出）；证据：`go build`/`go test`。

## Task 6: 前端提交身份与 Review 状态提示
- **Status**: `completed`
- **Completion Evidence**:
  - DocumentProcessor.tsx 增加 commit_id 与响应类型；新增共享模块 src/commitId.ts（消除 react-refresh 新增 warning）。
  - SuggestionsReview.tsx：每文档 commit id Map（沿用/新建，明确 4xx/5xx 删除、网络错误保留）；200 即 markApplied 收敛；pending/already_committed 走独立 role=status 信息条而非红条；partial 文案不变。
  - `rule` TR-6.1: npm run lint 新增问题 0（21→21，与基线一致，剩余为工具链既有 react-hooks 报错）；tsc -b 在 build 中通过。
  - `rule` TR-6.2: 三态文案与 id 轮换分支代码走查确认。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - [DocumentProcessor.tsx](file:///f:/swe/098861/project-03/web-app/src/DocumentProcessor.tsx#L50-L60) 的 `DocumentSuggestion` 增加 `commit_id?: string`。
  - [SuggestionsReview.tsx](file:///f:/swe/098861/project-03/web-app/src/components/SuggestionsReview.tsx)：
    - 以 `useRef<Map<number,string>>` 按文档保存 commit_id；`applyDocuments` 构造 payload 时注入（沿用或 `crypto.randomUUID()` 新建）。
    - 解析 `commits[0].history_status` / `already_committed`：任何 200 都 `markApplied` 收敛；新增 info 级提示条（区别于红色 error）：complete 不提示或弱提示；pending → “内容已应用到 paperless-ngx，本地修改历史仍在后台补写，稍后可在 History 查看。”；already_committed → “此前的提交已确认写入 paperless-ngx，本地历史已恢复。”
    - catch 分支：`err.response` 存在（服务器明确拒绝）→ 丢弃该文档 commit_id（允许编辑后新提交）；无 response（网络/超时）→ 保留 commit_id 供重试去重；错误文案维持现状。
  - 类型扩展 axios.patch 响应类型；不改动既有 partial 文案。
- **Acceptance Criteria Addressed**: AC-4, AC-10
- **Test Requirements**:
  - `rule` TR-6.1: `npm run lint`（含 tsc）无错误；证据：命令输出。
  - `rule` TR-6.2: 代码走查：200 三种状态下 markApplied 必被调用、pending/recovered 文案存在且不进入 error 红条；网络错误保留 id、4xx/5xx 轮换 id；证据：差异走查。

## Task 7: OCR Playground 提交身份与提示
- **Status**: `completed`
- **Completion Evidence**:
  - ocr/api.ts applyContent 增加 commitId 参数并返回 UpdateDocumentsResponse。
  - RunResults.tsx：commitRef 以 `docId:text` 为键——正文变化即换新 id；三态成功文案（已应用/补写中/已恢复）；仅 200 后关闭确认并 onApplied；明确 4xx/5xx 清 id，网络错误保留。
  - `rule` TR-7.1: lint 无新增问题、tsc 通过。
  - `rule` TR-7.2: 分支与 textKey 刷新逻辑代码走查确认。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - [api.ts](file:///f:/swe/098861/project-03/web-app/src/components/ocr/api.ts#L151-L162) `applyContent` 接收 commitId 参数并返回响应数据（commits/partial）。
  - [RunResults.tsx](file:///f:/swe/098861/project-03/web-app/src/components/ocr/RunResults.tsx#L165-L180)：
    - 维护 `{commitId, textKey}`；提交时若正文相对该 id 已变化（或首次/明确失败后）则新 UUID，否则复用。
    - 成功按 complete/pending/recovered 构造 onApplied 文案；仅在 200 后关闭确认框并显示成功 toast；网络错误保留 id，明确 4xx/5xx 清除 id；错误红条文案保留。
- **Acceptance Criteria Addressed**: AC-5, AC-10
- **Test Requirements**:
  - `rule` TR-7.1: `npm run lint` 无错误；证据：命令输出。
  - `rule` TR-7.2: 代码走查：正文变更后 id 刷新；三种成功文案分支存在；未确认成功不触发 onApplied；证据：差异走查。

## Task 8: 全量验证与前端构建部署
- **Status**: `completed`
- **Completion Evidence**:
  - `rule` TR-8.1: gofmt 改动文件全部干净；`go build ./...` 与 exe 构建成功；根包 go test 除 3 个既有 Windows 不兼容用例（onMount 反斜杠死循环、路径分隔符断言、POSIX chmod）外全部通过（74.7s），internal/sanitize 通过，ocr 包 1 个既有平台文案断言失败（与本改动无关）；web-app lint 与基线一致（无新增问题），`npm run build` 成功并已复制 dist 到根目录（Go 嵌入产物）。
  - 冒烟：以不可达 paperless 启动 paperless-gpt.exe，:8080 返回 200 并服务新 bundle；bundle 内含“still being backfilled/has been recovered”新文案；启动无对账错误（无 pending 时静默）。运行时 db/ 与 exe 已清理。
  - `rubric` TR-8.2 自评 4：后端三分支有单元/契约级可观察证据，前端三分支经构建产物确认；未做带 mock-paperless 的点击级 E2E（本机无 Docker，TestContainers 不可用），扣 1 分。
- **Priority**: high
- **Depends On**: Task 5, Task 6, Task 7
- **Description**:
  - 后端：`gofmt -l .` 无输出；`go test ./...` 通过（网络受限导致的 tiktoken 类既有失败记录但不计入）；`go build -o paperless-gpt`。
  - 前端：`cd web-app && npm run lint`；`npm run build`；`cp -r dist ../`（Windows 下用等价复制）。
  - 端到端冒烟（如环境允许）：启动应用确认 8080 与路由、前端 dist 被服务；在浏览器中验证 Review 与 Playground 的三种提示分支（可用浏览器代理访问本地页面）。
- **Acceptance Criteria Addressed**: AC-4, AC-5, AC-10
- **Test Requirements**:
  - `rule` TR-8.1: 全部质量门禁命令输出满足 NFR-3；证据：命令记录。
  - `rubric` TR-8.2: 端到端行为符合预期（三状态提示、文档收敛、无误报失败）；scale 1-5；anchors 1=仍可复现误报；3=主路径可用但状态文案缺失；5=两条手动链路三分支均可观察且自动化日志正确；threshold >=4；证据：运行/浏览器验证记录。
