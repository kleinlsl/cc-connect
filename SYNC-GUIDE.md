# CC-Connect Fork 同步与开发指南

> 本文档记录了如何从上游仓库同步更新到 fork 仓库，如何合并到本地开发分支，
> 以及当前开发分支的功能说明和运行方式。

---

## 目录

1. [仓库关系](#1-仓库关系)
2. [同步上游仓库到 Fork](#2-同步上游仓库到-fork)
3. [合并上游更新到开发分支](#3-合并上游更新到开发分支)
4. [修复合并冲突](#4-修复合并冲突)
5. [编译与运行](#5-编译与运行)
6. [当前开发分支功能说明](#6-当前开发分支功能说明)
7. [常用命令速查](#7-常用命令速查)

---

## 1. 仓库关系

```
上游仓库 (upstream)     你的 Fork (origin)          本地开发分支
chenhg5/cc-connect  →  kleinlsl/cc-connect  →  feat/card-message-and-thread-session
     ↑                      ↑                          ↑
   官方代码              同步后的 fork              魔改 + 合并上游
```

**远程仓库配置：**

```bash
origin    git@github.com:kleinlsl/cc-connect.git      # 你的 fork
upstream  git@github.com:chenhg5/cc-connect.git       # 上游官方
```

---

## 2. 同步上游仓库到 Fork

### 2.1 添加 upstream 远程仓库（首次）

```bash
cd ~/tujia_workspace/cc-connect
git remote add upstream git@github.com:chenhg5/cc-connect.git
```

### 2.2 拉取上游最新代码

```bash
# 拉取上游所有分支和标签
git fetch upstream

# 切到 main 分支并合并上游
git checkout main
git merge upstream/main
```

### 2.3 推送到你的 Fork

```bash
git push origin main
```

> **注意：** 每次上游有更新时，重复以上步骤即可。

---

## 3. 合并上游更新到开发分支

### 3.1 切到开发分支

```bash
git checkout feat/card-message-and-thread-session
```

### 3.2 合并上游 main

```bash
git merge upstream/main
```

如果出现冲突，需要手动解决（见第 4 节）。

### 3.3 推送到 Fork

```bash
git push origin feat/card-message-and-thread-session
```

---

## 4. 修复合并冲突

本次合并（v1.4.0 → v1.5.0）产生了一个冲突文件：

### 4.1 冲突文件：`platform/feishu/feishu.go`

**冲突 1 — 结构体字段（约第 190 行）：**

```go
<<<<<<< HEAD
	// 你的分支新增：ack 节流 + thread ID 别名
	ackThrottle    sync.Map
	threadIDAliases sync.Map
=======
	// 上游新增：图片批量合并
	imageBatchMu     sync.Mutex
	imageBatch       map[string]*imageBatchEntry
	imageBatchWindow time.Duration
>>>>>>> upstream/main
```

**解决：** 两边都保留，合并为：

```go
	ackThrottle     sync.Map
	threadIDAliases sync.Map
	imageBatchMu     sync.Mutex
	imageBatch       map[string]*imageBatchEntry
	imageBatchWindow time.Duration
```

**冲突 2 — 函数定义（约第 1170 行）：**

你的分支新增了 `shouldSendAck`、`sendAckMessage`、`sendAckAndGetThreadID` 等 ack 相关函数，
上游新增了 `bufferImage`、`flushImageBatchByRef` 等图片批量合并函数。

**解决：** 两组函数都保留。

### 4.2 测试文件适配

上游修改了 `dispatchMessage` 函数签名，新增了 `threadID` 参数：

```go
// 上游签名（12 个参数）
func (p *Platform) dispatchMessage(ctx, msgType, content string, mentions,
    messageID, sessionKey, userID, chatID string, rctx replyContext,
    parentID, threadID string, createTimeMs int64)
```

需要在所有测试调用中补上 `""` 作为 `threadID` 参数。

### 4.3 测试兼容修复

合并后发现 5 个测试失败，原因是你的分支在 `dispatchCoreMessage` 中加入了 ack 自动回复逻辑，
而上游的测试 mock 没有处理 reply API 调用：

| 测试 | 问题 | 修复 |
|------|------|------|
| `TestDispatchMessageIncludesQuotedImage` | ack reply 触发未 mock 的 API | mock 增加 reply 路径 |
| `TestFeishu_ThreadIsolation...` | dedup 字段未初始化 | 添加 `dedup: &core.MessageDedup{}` |
| `TestFeishu_HybridGroupStart...` | dedup 字段未初始化 | 添加 `dedup: &core.MessageDedup{}` |
| `TestLark_GroupReplyAll...` | ack 打到真实 API | 新增完整 mock server |
| `TestAllowChat_FiltersGroupMessages` | 同上 | 新增完整 mock server |

---

## 5. 编译与运行

### 5.1 编译

```bash
cd ~/tujia_workspace/cc-connect

# 编译 arm64 版本（macOS Apple Silicon）
GOOS=darwin GOARCH=arm64 go build -o cc-connect-arm64 ./cmd/cc-connect
```

### 5.2 运行方式

本地通过 **macOS launchd** 服务管理：

```bash
# 服务配置文件
~/Library/LaunchAgents/com.cc-connect.service.plist
```

**plist 关键配置：**

```xml
<key>ProgramArguments</key>
<array>
    <string>/Users/qitmac001720/tujia_workspace/cc-connect/cc-connect-arm64</string>
</array>
<key>WorkingDirectory</key>
<string>/Users/qitmac001720/.cc-connect</string>
<key>RunAtLoad</key>
<true/>
<key>KeepAlive</key>
<dict>
    <key>SuccessfulExit</key>
    <true/>
</dict>
```

- **RunAtLoad** = true → 登录后自动启动
- **KeepAlive** → 进程退出后自动重启
- **配置文件** → `~/.cc-connect/config.toml`
- **日志** → `~/.cc-connect/logs/cc-connect.log`

### 5.3 重启服务

```bash
# 编译后重启
GOOS=darwin GOARCH=arm64 go build -o cc-connect-arm64 ./cmd/cc-connect
launchctl stop com.cc-connect.service
# launchd 会自动重新启动，无需手动 start
```

### 5.4 服务管理命令

```bash
# 查看状态
launchctl list com.cc-connect.service

# 停止
launchctl stop com.cc-connect.service

# 卸载（彻底停止）
launchctl unload ~/Library/LaunchAgents/com.cc-connect.service.plist

# 重新加载（修改 plist 后）
launchctl load ~/Library/LaunchAgents/com.cc-connect.service.plist
```

---

## 6. 当前开发分支功能说明

**分支名：** `feat/card-message-and-thread-session`

**基于：** upstream/main (v1.5.0) + 自定义魔改

### 6.1 核心功能

#### 🧵 Thread 会话隔离

在飞书群聊中，使用 **reply in thread** 策略实现会话隔离：

- 每个用户在群聊中的消息通过 thread 隔离，不同 thread 对应不同会话
- 首条消息：发送 ack → 获取 real thread_id → 用 thread_id 作为 session key
- 后续消息：直接使用 thread_id 作为 session key
- 避免了之前用 msg_id 作为临时 thread ID 导致的多会话问题

#### 💬 ACK 自动回复

收到消息后自动发送确认回复，告知用户消息已收到：

- 基于消息内容的智能 ack（报警 → "收到报警，正在排查中..."，订单 → "收到，正在查询中..."）
- 30 秒节流，避免短时间内重复 ack
- thread 策略下空消息也发 ack（用于创建 thread）
- ack 消息附带 sessionKey，方便追踪

#### 🏷️ Thread ID 别名

维护 threadIDAliases 映射表，记录 Feishu 消息 ID 与真实 thread_id 的对应关系：

- 首条消息的 msg_id → real thread_id
- ack 消息的 message_id → real thread_id
- 用于 session 路由，不用于 quote 展开

#### 📎 Quote 上下文注入

- thread 内一律跳过 quote（避免重复上下文）
- 显式 @bot 回复时即使在 thread 内也获取 quote 上下文
- 修复了 quote 注入检查使用实际 thread_id 而非 sessionKey 格式

#### 👤 allow_p2p_from 配置

支持 `group_only` 模式下白名单用户可以单聊：

```toml
[projects.platforms.options]
allow_from = "group_only"
allow_p2p_from = "user_id_1,user_id_2"
```

### 6.2 修改的文件

| 文件 | 修改内容 |
|------|----------|
| `platform/feishu/feishu.go` | ack 逻辑、thread session 流程、threadIDAliases、allow_p2p_from |
| `platform/feishu/feishu_test.go` | 适配 dispatchMessage 新签名 |
| `platform/feishu/platform_test.go` | 新增 thread 隔离相关测试 |
| `config.example.toml` | 新增 allow_p2p_from 配置说明 |
| `docs/feishu.md` | 文档更新 |

### 6.3 配置示例

```toml
[[projects]]
  name = "workspace"

  [projects.agent]
    type = "claudecode"
    [projects.agent.options]
      allowed_tools = ["Read", "Grep", "Glob", "Bash", "Edit", "Write"]
      mode = "auto"
      work_dir = "/path/to/workspace"

  [[projects.platforms]]
    type = "feishu"
    [projects.platforms.options]
      allow_from = "*"
      app_id = "cli_xxx"
      app_secret = "xxx"
      enable_feishu_card = true
      session_key_strategy = "thread"    # 使用 thread 隔离
      thread_isolation = true            # 启用 thread 会话隔离
```

---

## 7. 常用命令速查

```bash
# ========== 同步上游 ==========
git fetch upstream
git checkout main && git merge upstream/main && git push origin main

# ========== 合并到开发分支 ==========
git checkout feat/card-message-and-thread-session
git merge upstream/main
# 解决冲突后：
git add -A && git commit -m "merge: 合并 upstream/main"
git push origin feat/card-message-and-thread-session

# ========== 编译 ==========
cd ~/tujia_workspace/cc-connect
GOOS=darwin GOARCH=arm64 go build -o cc-connect-arm64 ./cmd/cc-connect

# ========== 重启服务 ==========
launchctl stop com.cc-connect.service

# ========== 运行测试 ==========
go test ./platform/feishu/... -count=1
go build ./...
go vet ./...
```
