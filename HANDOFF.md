# HANDOFF — cc-connect

更新时间：2026-09-06 10:50。仓库：origin=kleinlsl/cc-connect（fork），upstream=chenhg5/cc-connect（主仓库）。

## 当前目标
1. **【已修复并上线】问题 A**：Hermes(ACP) 长任务结束后「最终回复在飞书发两遍」。三层修复 + 回归测试完成，全量测试全绿，已交叉编译部署（PID 5849，projects=4）。
2. **【待用户拍板·未修】问题 B**：ACP 状态行 `in/cr` 显示整轮累计值（出现 in 21.1M，超 1M 窗口）；ctx% 正常。用户已明确压缩边界 7%↔47% 瞬时错位「不改」。
3. 本分支全部源码改动**仍未 commit / push**（等用户指令，建议拆 commit）。

## 当前分支
`sync-upstream-0905`（基于 sync-upstream-0827；0827 保持不动可回退）。本地分支，**未 push**。

## 当前 commit
- HEAD=`6f8c7a79`（message "1"，只提交了编译产物 cc-connect-arm64，勿效仿）。
- 父 `9212f834`=上游 merge（v1.5.1-beta.1）；对 upstream/main 领先 39 / 落后 0。

## 已完成
1. **上游同步**（9212f834，33 文件）：feishu.go 我方 ack/thread 隔离/allow_p2p/卡片超链接/话题回显与上游群聊历史、大资源 Range 下载均保留。
2. **ack 开关** `ack_show_session_key`（默认 true；hermes-tujia 配 false 隐藏 `[session:..]`），含回归测试，已上线。
3. **ACP Hermes 回复状态行**（已上线）：`agent/acp/session.go` 报 usage/model；`core/engine.go` 方法版 `buildClaudeStatusLineFooter`（无 cw/cr 时省略该段，Claude 零变化）；文档 4.12/4.13。
4. **问题 A：outbox 重复发送根治（本次，三层）**
   - **根因**：即时发送（sendFinalWithOutbox）与后台 replayer（sweep）是两个并发 goroutine，对同一回复无互斥、无幂等，只靠 Add 时 `NextAt=now+30s` 初始 grace 时间窗防撞；Hermes 长 turn + 本机慢发送跨过该窗，sweep 用独立路径再发一次，飞书无幂等去重 → 两条。样本 ob id `ob-8bd5b95834567ce1`。
   - **第 1 层 进程内互斥（`core/outbox.go`）**：OutboxItem 加 `immediateHeld bool json:"-"`；Add 即由即时路径持有，`dueLocked` 跳过 held 项（与发送耗时解耦，不再赌时间窗）；即时路径可重试失败 `Fail` 才释放交接 replayer，成功 `Complete` 删除；held **不持久化**，进程崩溃重启自动释放、恢复补发不受影响。
   - **第 2 层 平台幂等（飞书 uuid）**：OutboxItem 加持久化 `UUID`（Add 时 `uuid.NewString()`）；新增 `core/outbox_ctx.go`（`WithOutboxUUID`/`OutboxUUIDFromContext`）；engine 即时路径与 replay 路径都把同一 uuid 注入 ctx；`platform/feishu/feishu.go` 在 `replyMessage`/`createMessage` 底层出口把 uuid 设进请求体 `body.Uuid`（飞书相同 uuid 1h 内至多成功一次）。非 outbox 发送 ctx 无 uuid，行为不变。
   - **第 3 层 日志 + 回归测试**：Add 补 `outbox: final reply parked for immediate send`（id/uuid/platform/session）；新增单测见下。
5. **顺带根治测试关停竞态（本次，被 outbox 放大暴露）**：`Engine.Stop` 原先 cancel 后不等待后台 goroutine，测试 `t.TempDir` 清理时仍有写盘 → 偶发 `TempDir RemoveAll: directory not empty`（功能断言从不受影响、生产无此场景）。三处治理：
   - `core/outbox.go` 新增 `Stop()`（内部 runCtx/runDone，停 replayer 并等其退出；stopped 后 persist/remove 跳过磁盘 I/O）。
   - `core/engine.go` 加 `turnWg sync.WaitGroup`，3 处 `go processInteractiveMessageWith` 全部纳入；`Stop` 顺序=cancel→`sessions.StopPersistence()`→停 platform→关 agent session→`turnWg.Wait()`→…→最后 `outbox.Stop()`。
   - `core/session.go` 加 `StopPersistence()`（stopped 后 `saveLocked` 直接 no-op，任何后台 goroutine 都无法在关停后写 sessions.json）。
   - `core/cuj_test.go`：13 处 `NewEngine` 统一补 `t.Cleanup(func(){ x.Stop() })`（**注意：在 newCUJEnv 这类 helper 里必须用 t.Cleanup 而非 defer，defer 会在 helper 返回时就 Stop，导致 engine 当场报废、所有用例收不到回复——已踩过并修正**）。

## 未完成
### B. ACP 状态行 in/cr 累计虚高（in 21.1M）
- 根因方向：`agent/acp/session.go::absorbPromptUsage` 取 session/prompt 终态 usage，疑似整 turn 多次子调用累计；应对标 claudecode 只用最后一次单次值。下一步读 `~/.hermes/hermes-agent/acp_adapter/server.py`（约 917-923、345-357）确认，查 ACP session/update 有无 per-call usage。ctx 通道不动。改不改待用户拍板。
### C. 提交 / 推送 / 仓库卫生
- 全部未 commit；**切勿 `git add cc-connect-arm64*`（69MB 二进制）**。
- 建议拆 commit：ack 开关 / outbox 互斥+uuid+关停治理 / ACP resume·死锁 / ACP 压缩·model 透传 / ACP 状态行 / 文档。
- `git rm --cached cc-connect-arm64`+.gitignore 待拍板；`git push -u origin sync-upstream-0905` 待执行。

## 已修改文件（本次问题 A 相关，均未提交）
- 新增：`core/outbox_ctx.go`。
- M：`core/outbox.go`（held/UUID/Stop）、`core/engine.go`（uuid ctx 透传、turnWg、Stop 顺序、StopPersistence）、`core/session.go`（StopPersistence）、`platform/feishu/feishu.go`（body.Uuid 注入 + buildReplyMessageReqBody 加 uuid 形参）。
- 测试：`core/outbox_test.go`（ImmediateHoldBlocksSweep / ImmediateFailReleasesHoldForReplay / HoldReleasedAndUUIDPersistedAcrossRestart / UUIDUniquePerReply / OutboxCtxUUIDRoundTrip / StopQuiescesDisk；RunRedeliversOnKick 补 Fail 交接）、`core/session_test.go`（StopPersistenceFreezesFile）、`core/cuj_test.go`（13 处 t.Cleanup Stop）、`platform/feishu/platform_test.go`（TestBuildReplyBodyUUID + 旧用例补 uuid 实参）。
- 文档：`docs/hermes-acp-integration.zh-CN.md` 4.11 补「互斥+uuid 防重复」、TL;DR 第 11 条。
- 更早未提交改动见 git status（agent/acp/*、config/*、i18n/interfaces 等）。
- **临时文件待删（勿提交）**：`.tmp_patch_*.py`、`core/*.bak-prehold`、`platform/feishu/*.bak-prehold`；二进制备份 `cc-connect-arm64.bak-*` 不入库。

## 下一步（按优先级）
1. 飞书「卢松林助手通知」群跑一个 5 分钟以上长任务，实测最终回复**只回一条**（无法代发，需用户实测）；可 grep `~/.cc-connect/logs/cc-connect.log` 的 `outbox:` 与 uuid。
2. 用户拍板问题 B 是否修。
3. 拆 commit、push；按需二进制脱钩。

## 验证命令
```bash
go build ./... && go vet ./core/ ./platform/feishu/
go test ./... -p 1
go test ./core/ -run TestCUJ -count=2
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o cc-connect-arm64.new ./cmd/cc-connect
# 部署：cp -p 备份 → mv .new 原子替换 → xattr -d quarantine，然后：
launchctl bootout gui/$(id -u)/com.cc-connect.service
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.cc-connect.service.plist
launchctl kickstart -k gui/$(id -u)/com.cc-connect.service
grep "is running" ~/.cc-connect/logs/cc-connect.log # projects=4
```

## 验证结果（2026-09-06 10:46）
- gofmt 干净；`go build ./...`、vet 通过。
- `go test ./... -p 1` **EXIT=0，45 个包全 ok**（此前的 media_pipeline / CUJ TempDir flaky 已由关停治理根治：media 用例 count=30 全过、CUJ count=2 全过）。
- outbox/feishu/session 新增回归全部通过；CUJ 通过。
- 交叉编译 arm64 成功（Mach-O arm64，69M）；备份 `cc-connect-arm64.bak-20260906-104627`，原子替换后重启，新 PID 5849，10:46:49 `cc-connect is running projects=4`，4 个 project engine started，cron/timer/bridge/api 正常，单进程，无启动 panic。

## 风险 / 注意事项
- 工作区大量未提交改动，切勿 reset / 切分支，避免丢失。
- outbox 原则：「不丢」优先于「不重」；本次是在保留 at-least-once 落盘补发骨架的前提下补全互斥+幂等，不是推翻。
- `Engine.Stop` 现在会 `turnWg.Wait()` 等待在途消息处理收敛（由 cancel + 关 agent session 驱动退出）；若未来新增不响应 ctx 的处理 goroutine，需一并纳入 turnWg，否则 Stop 可能变慢。
- 重启会 kill 在跑 hermes/claude 子进程，挑群里无在跑会话时。
- **不改 `~/.hermes` 第三方源码**（用户明确）。
- 日志盲区：cc 运行期看不到 Hermes 逐行 API call；Hermes 侧看 `~/.hermes/profiles/tujia/logs/agent.log` 与只读 `state.db`。
- 4 个 project：workspace=claudecode、tujia-feishu-agent-claude=claudecode、opencode-workspace=acp、hermes-tujia=acp；顶层 idle_timeout_mins=120。
- 关键路径：plist `~/Library/LaunchAgents/com.cc-connect.service.plist`；运行配置 `~/.cc-connect/config.toml`（含 secret，不入库）；socket `~/.cc-connect/run/api.sock`；outbox 目录 `~/.cc-connect/outbox/`（正常应为空）。
- 遗留运维：飞书 Hermes 应用 cli_aaf783e8d5b99d1d 需补 `im:message.group_msg` 并发版（否则收不到不 @ 群消息，API 230027）；本机 :3003 网关 prefill 慢/偶发缓存不命中 cc 侧无解；1M 长上下文需网关侧加 [1M] 渠道。
- 任何文档/提交不得写入 app_secret 等凭证明文。
