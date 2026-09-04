# Hermes 通过 ACP 接入 cc-connect 与飞书排障记录

> 本文沉淀一次完整的实战过程：把 **Hermes Agent**（多 profile）通过通用 **ACP adapter**
> 接入 cc-connect，并在飞书侧陆续解决「引用卡片丢超链接 / 消息被原生 gateway 截胡 /
> 转发卡片不自动处理 / 思考过程泄漏 / 话题内看不到引用原文」五个问题。
> 适用对象：后续在 cc-connect 上接入 Hermes 或其它 ACP 兼容 Agent、以及排查飞书消息
> 链路问题的同学。
>
> 涉及代码：`agent/acp/`、`platform/feishu/feishu.go`；涉及配置：`~/.cc-connect/config.toml`、
> `~/.hermes/profiles/<profile>/config.yaml`。

---

## 目录

1. [结论速览（TL;DR）](#1-结论速览tldr)
2. [Hermes 通过 ACP 接入](#2-hermes-通过-acp-接入)
3. [多 profile 与多项目](#3-多-profile-与多项目)
4. [问题排查与修复记录](#4-问题排查与修复记录)
5. [群消息响应的三档模型](#5-群消息响应的三档模型)
6. [本次新增 / 涉及的配置项](#6-本次新增--涉及的配置项)
7. [排障方法论与常用命令](#7-排障方法论与常用命令)
8. [测试与验证清单](#8-测试与验证清单)
9. [遗留事项](#9-遗留事项)

---

## 1. 结论速览（TL;DR）

| # | 现象 | 根因 | 解法 |
|---|---|---|---|
| 1 | **引用**一张交互卡片，提取出的文本里超链接全丢 | 引用走 `raw_card_content`，链接在独立字段且 `url` 是**对象** `{"url":"..."}`；旧代码 `x["url"].(string)` 只认字符串，断言失败被静默跳过 | 统一走 `extractURLFromLinkValue()`，字符串/对象都认（见 [4.1](#41-引用交互卡片超链接丢失)） |
| 2 | Hermes 没有 ack、消息像没走 cc-connect | Hermes **自带飞书 gateway** 把消息截走了 | Hermes profile 的 `platforms.feishu.enabled=false` 并重启其 gateway（见 [4.2](#42-消息被-hermes-原生飞书-gateway-截胡)） |
| 3 | 转发卡片到群里，机器人不自动处理（Claude 可以） | Hermes 飞书应用**缺 `im:message.group_msg` 权限**，飞书服务端压根不推送「不 @机器人」的群消息 | 飞书后台补权限并发版；代码侧已就绪（见 [4.3](#43-转发卡片不自动处理飞书权限)） |
| 4 | Hermes 把「思考过程」也发到了飞书 | Hermes 发的是 `agent_thought_chunk`（thought），代码白名单写的是 `agent_thinking_chunk`（thinking），没匹配上被当成**普通正文**发出 | 补全思考事件名拼写（见 [4.4](#44-思考过程泄漏到飞书)） |
| 5 | 手动引用会新开话题，被引用的原卡片在话题外、不好对照 | thread 策略以「用户这条引用消息」为根建新话题，原消息本就不属于该话题 | 新增 `echo_quoted_in_thread`，建话题时把引用原文回显进话题（见 [4.5](#45-话题内看不到引用原文echo_quoted_in_thread)） |
| 6 | 新话题的 ack 上 `[session:]` 先是 `om_`（消息 id），随后又变成 `omt_`（话题 id），像开了两个会话 | ack 必须先发出去、飞书才生成话题 id，所以 ack 落字时只能印临时 key；真实会话其实只有一个；且飞书 PATCH 只能改卡片、改不了文本 ack | 新话题首条 ack 不显示临时 key（`ackSessionFooter`），真实 `omt_` 在回复 footer 体现（见 [4.6](#46-新话题-ack-上的-session-先-om_-后-omt像两个会话)） |
| 7 | 同一话题，Hermes 端却出现两个 ACP 会话 id（短时间正常、重启或隔 ~120 分钟后分裂） | **cc 通用 ACP adapter bug**：ACP 规范里 `session/load` 响应不回 `sessionId`，cc 旧代码却要求其非空、否则静默 `session/new`，把 Hermes 已恢复的会话丢弃（非 Hermes 问题，Claude 走专用 adapter 不受影响） | 已修：load 无错即成功、沿用请求的 resume id；冷启动（重启或顶层 `idle_timeout_mins=120` 空闲关闭）后复用同一 Hermes 会话（见 [4.7](#47-同一个话题hermes-端却出现两个-acp-会话)） |

> 经验：飞书消息问题要先区分**「飞书有没有把消息推过来」**和**「cc-connect 收到后怎么处理」**
> 两层——前者是应用权限/事件订阅，后者才是代码与配置。很多「代码没问题但就是不触发」
> 的情况都卡在第一层。

---

## 2. Hermes 通过 ACP 接入

### 2.1 原理

Hermes 原生实现了 [Agent Client Protocol](https://agentprotocol.ai/)（ACP）。cc-connect 的
`agent/acp/` 是通用 ACP 适配器，通过 **stdio + JSON-RPC** 拉起任意 ACP 兼容 Agent，因此
**不需要为 Hermes 单独写 adapter**，直接复用 `type = "acp"`：

```
飞书 → cc-connect(platform/feishu) → core.Engine → agent/acp → `hermes -p <profile> acp`(stdio)
```

- 启动命令：`hermes -p <profile> acp`
- 握手：`protocolVersion: 1`，cc-connect ACP adapter 兼容
- 注意：**必须用管道接 stdio**；把 stdio 重定向到文件会导致 hermes acp 崩溃。

### 2.2 最小 project 配置

在 `~/.cc-connect/config.toml` 增加一个 project：

```toml
[[projects]]
  name = "hermes-tujia"
  show_context_indicator = true
  reply_footer = true
  inject_sender = false

  [projects.agent]
    type = "acp"                       # 关键：走通用 ACP 适配器
    [projects.agent.options]
      cmd = "hermes"
      args = ["-p", "tujia", "acp"]   # -p 指定 profile
      display_name = "Hermes (tujia)"
      mode = "default"                # ACP 模式，见 2.3
      reset_on_idle_mins = 720
      work_dir = "/path/to/shared/workspace"

  [[projects.platforms]]
    type = "feishu"
    [projects.platforms.options]
      group_only = true
      allow_from = "*"
      allow_p2p_from = "ou_xxx,ou_yyy"      # 允许私聊的用户 open_id（逗号分隔）
      allow_chat = "oc_xxx,oc_yyy"          # 允许响应的群 chat_id（逗号分隔）
      app_id = "cli_xxx"
      app_secret = "<your-app-secret>"
      enable_feishu_card = true
      progress_style = "card"
      reaction_emoji = "OnIt"
      send_file = true
      session_key_strategy = "thread"
      thread_isolation = true
      echo_quoted_in_thread = true         # 本次新增，见 4.5
```

### 2.3 关于 `mode`（与 Claude Code 的区别）

ACP adapter 仅支持四种权限模式：`default` / `acceptEdits` / `plan` / `bypassPermissions`。
**没有 `auto`**——`auto` 是 claudecode adapter 的专用值，不能照搬到 ACP。要「自动放行」
用 `bypassPermissions`，常规用 `default`。

---

## 3. 多 profile 与多项目

Hermes 的 profile 与 cc-connect 的 project 是**一一对应**关系：

- 每个 Hermes profile 是 `~/.hermes/profiles/<profile>/` 下的一套独立配置（模型、工具、飞书等）。
- 在 cc-connect 里，每个要用的 profile 配一个 `[[projects]]`，用 `args = ["-p","<profile>","acp"]` 区分。
- 「多 profile + 多项目」就是配多个 project，各自指定：
  - 不同的 `args`（profile）
  - 不同的 `work_dir`（项目工作目录，可相同也可不同）
  - 不同的飞书机器人 `app_id/app_secret`（一个机器人对应一个 project，避免串会话）

```toml
# 示意：两个 profile = 两个 project = 两个飞书机器人
[[projects]]
name = "hermes-a"
[projects.agent]; type="acp"; args=["-p","profileA","acp"]; work_dir="/proj/a"
[[projects.platforms]]; type="feishu"; options.app_id="cli_botA" ...

[[projects]]
name = "hermes-b"
[projects.agent]; type="acp"; args=["-p","profileB","acp"]; work_dir="/proj/b"
[[projects.platforms]]; type="feishu"; options.app_id="cli_botB" ...
```

> cc-connect 启动后日志出现 `cc-connect is running projects=N` 即表示 N 个 project 都起来了。

---

## 4. 问题排查与修复记录

### 4.1 引用交互卡片超链接丢失

#### 两种卡片来源，结构不同

- **直接推送 / 单条转发**的卡片：链接已**内联在 markdown content** 里（`[文字](url)`），
  `extractInteractiveCardText` 直接能拿到。
- **引用（reply/quote）一张卡片**：走 `fetchSingleMessage()`（`platform/feishu/feishu.go`），
  带 query `?card_msg_content_type=raw_card_content`，拿到的是**原始 DSL**：
  - 外层 `body.content` 是 JSON 字符串，里面的 `json_card` **还是字符串，需要二次 JSON parse**；
  - 真正卡片在 `body/config/header/newBody/schema/source` 下，元素嵌套极深
    （`body.property.elements[].property.columns[].property.elements[]...`）；
  - 链接元素形如 `{"content":"Watcher链接","url":{"url":"https://..."}}`——**文字与 URL 是同级字段，URL 是对象**。

#### 根因

`walkCardValue` 只从 `content/text/title/plain_text` 取文本；第一版修复用
`x["url"].(string)` 取链接，但真实结构里 `url` 是 `map`（对象），字符串类型断言失败，
链接被静默丢弃，于是只剩文字没有 URL。

#### 修复

`applyCardLinks()` 中改为统一调用 `extractURLFromLinkValue()`，同时兼容
「URL 是字符串」和「URL 是 `{url/pc_url/ios_url/android_url}` 对象」；同时支持
`href` map、`multi_url` 等形态，把对应文本包装成 Markdown 链接 `[text](url)`。

真实雷达报警卡片验证：8 个链接（Watcher / 手机 Watcher / 跟进 / 手机跟进 /
屏蔽 6 小时 / 屏蔽 7 天 / 永久屏蔽 / 修改级别）全部恢复。

> 抓真实卡片结构的方法见 [7.3](#73-用飞书-openapi-拿真实卡片消息)。**务必用真实数据验证，
> 单测里手写的简化结构很容易和线上 DSL 不一致（本次第一版修复就栽在这）。**

### 4.2 消息被 Hermes 原生飞书 gateway 截胡

#### 现象

Hermes project 没有 cc-connect 的 ack（「收到，正在处理中…」），cc-connect 日志里
**连 `onMessage entry` 都没有**，像是消息根本没到 cc-connect；而 Claude project 正常。

#### 根因

Hermes Desktop / `hermes gateway run --profile <profile>` **自带一套飞书集成**，默认开启，
会用同一套飞书凭证直接接收消息——消息在到达 cc-connect 之前就被它接走了。

#### 解法

编辑 `~/.hermes/profiles/<profile>/config.yaml`：

```yaml
platforms:
  feishu:
    enabled: false      # 关掉 Hermes 原生飞书，让消息只走 cc-connect
```

重启该 profile 的 gateway，确认状态文件 `~/.hermes/profiles/<profile>/gateway_state.json`
里 `platforms.feishu.state = disconnected`，且 `feishu_seen_message_ids.json` 停止增长。

> 排查要点：cc-connect 的 ack 是 **platform 层逻辑，与 agent 类型无关**。Claude 有、Hermes
> 没有时，先别急着怀疑代码，先确认消息到底进没进 cc-connect（看有没有 `onMessage entry`）。

### 4.3 转发卡片不自动处理（飞书权限）

#### 现象

把一张卡片**转发**到群里、不 @机器人：Claude project 自动处理，Hermes 毫无反应
（同样连 `onMessage entry` 都没有）。

#### 两层模型（关键认知）

1. **飞书服务端推送层**：群聊里机器人**默认只收到 @它的消息**；要收到群里「不 @它」的
   所有消息（含转发卡片、他人对话），应用必须具备 **`im:message.group_msg`（获取群组消息）**
   权限。缺权限时服务端根本不推送，cc-connect 任何开关都无能为力。
2. **cc-connect 过滤层**：消息进来后，再由配置决定要不要响应（见第 5 节）。

#### 验证方法

用两个应用的凭证分别调「拉群历史」接口，有权限返回 `code=0`，缺权限返回
`230027 need scope: im:message.group_msg`：

```bash
TOKEN=$(curl -s -X POST https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal \
  -H "Content-Type: application/json" \
  -d '{"app_id":"cli_xxx","app_secret":"xxx"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["tenant_access_token"])')
curl -s "https://open.feishu.cn/open-apis/im/v1/messages?container_id_type=chat&container_id=oc_xxx&page_size=3" \
  -H "Authorization: Bearer $TOKEN"
```

本次对照：Claude 应用 `code=0`，Hermes 应用 `230027`，实锤是权限差异。

#### 解法（需要在飞书后台操作，代码无需再改）

1. 飞书开放平台 → 开发者后台 → 选中应用 →「权限管理」→ 开通 `im:message.group_msg`；
2.「版本管理与发布」→ 创建版本 → 发布 / 管理员审批通过后生效；
3. 生效后转发卡片即可自动处理（interactive 卡片默认免 @，见第 5 节）。

### 4.4 思考过程泄漏到飞书

#### 现象

Hermes 的内部「思考 / reasoning」被一段段发到了飞书群里；明明全局已配
`[display] thinking_messages = false` 却拦不住。

#### 根因

ACP 的 session/update 事件类型在不同 Agent 里命名不一。Hermes 实际发的是
**`agent_thought_chunk`**（thought，本次日志里 2180 条），而
`agent/acp/mapping.go` 的思考白名单写的是 **`agent_thinking_chunk`**（thinking）。
拼写不匹配 → 没归类为 `EventThinking` → 掉进兜底的「未知类型提取文本」分支 →
被当成 **`EventText`（正常回答）** 发出。而 `thinking_messages=false` 只能拦截被正确
标记为 `EventThinking` 的事件，所以形同虚设。

日志铁证：

```
msg="acp: session/update" ... "sessionUpdate":"agent_thought_chunk"
msg="acp: mapSessionUpdateFallback: extracted text from unknown format" kind=agent_thought_chunk
```

#### 修复

在思考类型分支补全拼写（并兼容变体）：

```go
case "reasoning", "reasoning_chunk", "thinking",
     "agent_thinking_chunk", "agent_thought_chunk", "thought", "thought_chunk":
    // → core.EventThinking（随后被 display.thinking_messages 开关控制）
```

> 通用教训：ACP 是开放协议，**不同 Agent 的事件名 / 字段结构可能有差异**，接入新 Agent
> 时先开 debug 统计它实际发出的 `sessionUpdate` 类型集合（命令见 7.4），再核对映射表。

### 4.5 话题内看不到引用原文（`echo_quoted_in_thread`）

#### 现象与根因

`session_key_strategy = "thread"` 下，引用一张不在任何话题里的卡片并 @机器人时，该消息
`thread_id` 为空，cc-connect 只能以「用户这条引用消息」为根，让飞书**新建话题**，后续
回复都挂在新话题；被引用的原卡片因此留在话题外。

> 注意：**Agent 其实看得到引用全文**——cc-connect 已抓取并以 `[Quoted message from …]`
> 注入给模型（`core.Message.ExtraContent`），问题只是「人在飞书话题 UI 里对照不到原文」。

#### 实现（方案 A）

新增 platform option `echo_quoted_in_thread`（默认 false，按需开启）。在
`dispatchCoreMessage()` 里，thread 策略发完 ack、拿到真实 `thread_id` 后，若满足：

1. 开关打开；
2. **首次新建话题**（临时 session key 的 thread 段 == 触发消息 id；已有话题不重复回显）；
3. `msg.ExtraContent` 非空；

则在话题内补发一条 `formatQuotedEcho()` 格式化后的「📎 引用内容（来自 xxx）：…」，
超 4000 字自动截断。最终话题内顺序为：**ack → 📎引用原文 → Agent 回复**，话题自包含。

### 4.6 新话题 ack 上的 session 先 `om_` 后 `omt`，像两个会话

#### 现象

在一个**全新话题**里发消息，ack「收到，正在处理中…」上印的是
`[session: feishu:oc_xxx:thread:om_<消息id>]`，而真正建立的会话 / 后续消息用的是
`[session: feishu:oc_xxx:thread:omt_<话题id>]`，两个 id 看起来像「先在一个会话、又新开一个」。

#### 这不是真的开了两个会话

- thread 策略下，新消息没有 `thread_id`，cc-connect 先用**触发消息 id（`om_`）**拼一个
  临时 session key 去发 ack；
- ack 调飞书 reply 接口后，飞书才返回真实**话题 id（`omt_`）**，随后 session key 更新为
  `omt_`，并通过 thread-id 别名机制把临时 key 归并过去；
- 因此 session 存档里 `omt_` 只对应**一个**会话，话题内多次发言都连续落在它上面，
  `om_` 临时 key 不会被存成独立会话。可用 `~/.cc-connect/sessions/<project>_*.json`
  的 `active_session` 核实。

#### 修复（消除显示不一致）

第一版尝试「reply 拿到真实 thread id 后，用飞书 `Im.Message.PATCH` 就地改写 ack 文本」，
但飞书返回 **`230001 This message is NOT a card`**——**PATCH 接口只能编辑卡片(interactive)
消息，不能编辑纯文本 ack**，此路不通。

最终方案（纯函数 `ackSessionFooter(sessionKey, triggerMessageID)`）：新话题首条 ack 的
session key 此刻注定是临时的、且文本消息事后无法编辑，因此**这种情况下干脆不附加
`[session:]` 页脚**（ack 只显示「收到，正在处理中…」）；当 key 已经是真实 `omt_`
（话题内续聊）时才显示页脚。真实会话 id 始终能在 Agent 回复的 footer 看到。

- 判定「临时 key」：`threadIDFromSessionKey(sessionKey) == triggerMessageID`（thread 段
  就是触发消息 id）。
- 单测 `TestAckSessionFooter` 覆盖「新话题隐藏 / 已有话题显示 / 空 key 隐藏 / 无触发 id
  兜底显示」；对应 HybridGroup 端到端用例的断言也改为「新话题 ack 不含 `[session:`」。

### 4.7 同一个话题，Hermes 端却出现两个 ACP 会话

#### 现象

飞书侧 session key 始终是同一个 `omt_`，但底层 Hermes ACP session id 中途换了
（`past_agent_session_ids` 里留着旧 id），Hermes 会话列表里出现两条。

#### 热路径 vs 冷启动：为什么短时间正常、隔久了才分裂

每条消息进来，cc 先看手里有没有**活着的底层 agent 会话**（`state.agentSession.Alive()`）：

- **活着（热路径）**：直接发 `session/prompt`，不握手、不 `session/load`、不 `session/new`，
  Hermes 端始终同一个 ACP 会话——所以短时间连续聊完全正常。
- **没了（冷启动）**：才 `StartSession(旧id)` → initialize → `session/load`，分裂只发生在这里。

底层会话变「没了」的两个触发点：

1. **进程重启 / 崩溃**：cc 重启会 signal-kill `hermes acp` 子进程、清空 live 会话。
2. **顶层 `idle_timeout_mins`（当前=120 分钟）**：一轮结束后满 120 分钟无新消息，cc 主动
   `closeAgentSessionWithTimeout` 关闭底层 live 会话（日志
   `agent session idle timeout: closing live agent session`），下次消息重新 resume。
   ——这就是「隔很久再聊，Hermes 端变两个会话」的日常触发点。

> 别和 project 级 `reset_on_idle_mins=720`（12 小时）混淆：后者是**连 cc 会话+历史一起清空
> 重来**（有意的上下文重置，会发提示）；`idle_timeout_mins` 只关底层 agent 会话、cc 历史保留。

#### 根因：cc 通用 ACP adapter 的 bug（不是 Hermes）

按 ACP 规范，**`LoadSessionResponse` 不返回 `sessionId`**（加载的是请求里指定 id 的会话，
id 客户端已知；官方 SDK `acp/schema.py` 该结构只有 `_meta/configOptions/models/modes`，
只有 `NewSessionResponse` 才有 `sessionId`）。而 cc 旧代码 `agent/acp/session.go` 要求 load
响应里 `sessionId != ""` 才算成功，否则**静默落到 `session/new`**：

- Hermes 其实已成功恢复：日志先 `Restored ACP session <id> from DB (N messages)` +
  `Loaded session <id>`，并按规范在 load 响应前回放历史；
- cc 解析出的 `sessionId` 恒为空 → 丢弃已恢复会话、再 `session/new`，于是 Hermes 端多出一条，
  被回放的历史还会变成 `drained stale events`。
- Claude 走专用 `claudecode` adapter、不经此通用路径，所以无此问题。

**修复**：`session/load` 无 error 即成功，id 沿用请求的 `resumeSessionID`（响应若回了
sessionId 则优先，兼容非标准 agent）。回归测试 `agent/acp/session_load_test.go`
（`TestHandshake_Resume*`，修复前得到 `brand-new-id` 失败、修复后沿用 resume id 通过）。

#### 实际影响

- 修复前：每次冷启动都白恢复一次再新建，靠 cc 自己 replay history 兜底，所以**文本对话不丢、
  模型不失忆**，但 Hermes 进程内状态（终端 shell、临时变量、子进程）被重置，且 Hermes 会话列表
  出现重复条目。
- 修复后：重启或 120 分钟空闲后的 resume 会真正复用 Hermes 恢复的同一会话，不再分裂。

#### 验证修复

1. 单测：`go test ./agent/acp/ -run TestHandshake_Resume -v`。
2. 端到端：重启服务（制造冷启动）后，在**已有话题**发一条消息，期望日志只有
   `Restored/Loaded session <同一个 id>`、**不再紧跟** `Created ACP session`；
   `~/.cc-connect/sessions/hermes-tujia_*.json` 里该会话的 `agent_session_id` 保持不变、
   `past_agent_session_ids` 不增长。
3. 空闲路径（可选）：把顶层 `idle_timeout_mins` 临时调小到 1，聊一句等 >1 分钟再发，确认复用
   同一 id 后改回 120。

---

## 5. 群消息响应的三档模型

都写在 `[projects.platforms.options]` 下。**前提：飞书应用能收到消息**（最保守档除外，
因为它本来就只收 @自己的消息，不依赖 `im:message.group_msg`）。

| 档位 | 配置 | 普通文本 | 卡片(interactive) | 适用 |
|---|---|---|---|---|
| 最宽松 | `group_reply_all = true`（或 `require_mention = false`） | 不用 @ | 不用 @ | bot 专用群，让它参与每句对话 |
| **默认（推荐）** | 两者都不配 | 需要 @ | **免 @ 自动处理** | 报警群：卡片自动处理、闲聊不打扰 |
| 最保守 | `card_requires_mention = true`（本次新增） | 需要 @ | **也要 @** | 想「一切消息都必须 @才回」 |

实现位置：`onMessage()` 群聊过滤 switch；`interactive` 分支条件为
`msgType == "interactive" && !p.cardRequiresMention`。

另有两个与 @ 无关的免 @ 通道：`@all`（`respond_to_at_everyone_and_here`）、以及
「已激活话题内的附件续发」（`thread_isolation` + 图片/文件等 attachment 类型）。

---

## 6. 本次新增 / 涉及的配置项

### 6.1 platform options（`[projects.platforms.options]`）

| 键 | 默认 | 说明 |
|---|---|---|
| `echo_quoted_in_thread` | `false` | thread 策略下，建新话题时把被引用原文在话题内回显一次（方案 A） |
| `card_requires_mention` | `false` | 为 `true` 时，群里的卡片也必须 @机器人才响应（关掉默认的卡片免 @） |
| `group_reply_all` / `require_mention=false` | `false` | 群里所有消息免 @（最宽松档） |

### 6.2 全局展示开关（`[display]`，控制思考/工具过程是否下发到 IM）

```toml
[display]
  thinking_messages = false   # 是否展示思考过程（默认 true）
  thinking_max_len = 30
  tool_messages = false       # 是否展示工具调用过程（默认 true）
  tool_max_len = 10
  # mode = "quiet"            # full(默认,全显示) / compact / quiet(后两者等价于关掉思考+工具)
```

> 4.4 的修复正是让 Hermes 的思考被正确归类为 thinking，从而能被这里的
> `thinking_messages=false` 拦下。`mode="quiet"` 会同时关闭思考与工具消息。

---

## 7. 排障方法论与常用命令

### 7.1 服务管理（launchd）

```bash
# 重启
launchctl kickstart -k gui/$(id -u)/com.cc-connect.service
# 看是否 N 个 project 都起来
grep "cc-connect is running" ~/.cc-connect/logs/cc-connect.log | tail -1
# 进程
ps aux | grep cc-connect-arm64 | grep -v grep
```

### 7.2 日志：临时开 debug

把 `~/.cc-connect/config.toml` 的 `[log] level` 在 `info`/`debug` 间切换后重启。
debug 下关键行：

- `feishu: onMessage entry`（含 `msg_type`）——判断**消息到底进没进来**；
- `passing interactive card message without mention` / `ignoring group message without bot mention`——过滤层决策；
- `acp: session/update`——ACP Agent 实际发出的原始事件；
- `mapSessionUpdateFallback: extracted text from unknown format`——未知事件被当文本（警惕泄漏）。

> debug 很啰嗦，排查完务必改回 `info`。

### 7.3 用飞书 OpenAPI 拿真实卡片消息

```bash
# 1) 拿 tenant_access_token（见 4.3）
# 2) 拉单条消息，引用卡片务必带 raw_card_content
curl -s "https://open.feishu.cn/open-apis/im/v1/messages/<om_xxx>?card_msg_content_type=raw_card_content" \
  -H "Authorization: Bearer $TOKEN"
# body.content -> json_card 是「字符串里的 JSON」，需要两次 json.loads
```

### 7.4 统计 ACP Agent 实际发出的事件类型

```bash
# 日志里 JSON 被转义（\"），统计 sessionUpdate 取值分布
python3 - <<'PY'
import re,glob,collections
c=collections.Counter()
for f in glob.glob('/Users/<you>/.cc-connect/logs/cc-connect.log*'):
    t=open(f,encoding='utf-8',errors='ignore').read()
    c.update(re.findall(r'sessionUpdate\\*":\s*\\*"([a-z_]+)\\*"', t))
print(c.most_common())
PY
```

### 7.5 看 cc-connect 实际送给 Agent 的内容

session 历史文件：`~/.cc-connect/sessions/<project>_*.json`，里面每个 session 的
`history[].content` 就是最终注入给 Agent 的文本（可确认引用内容 / 链接是否完整）。
注意：**旧 session 的历史不会因代码修复而重新提取，验证修复要发一条新消息**。

### 7.6 编译 arm64 二进制

```bash
cd ~/tujia_workspace/cc-connect
gofmt -w <改动文件>
go test ./agent/acp/ ./platform/feishu/
GOOS=darwin GOARCH=arm64 go build -o cc-connect-arm64 ./cmd/cc-connect
launchctl kickstart -k gui/$(id -u)/com.cc-connect.service
```

---

## 8. 测试与验证清单

按 `AGENTS.md` 要求，bugfix 必须带「修复前失败、修复后通过」的回归测试。本次新增：

- `agent/acp/mapping_test.go`
  - `TestMapSessionUpdate_agentThoughtChunk`：`agent_thought_chunk/thought/thought_chunk`
    必须映射为 `EventThinking`（旧代码会得到 `EventText`，测试先红后绿）。
- `platform/feishu/feishu_test.go`
  - 卡片链接：`TestExtractInteractiveCardText_URLObject`、`..._MultipleURLObjects`
    （url 为对象、深层嵌套、多链接），以及此前的 href/button/multi_url 用例；
  - `TestFormatQuotedEcho`：引用回显的格式化 / 去前缀 / reply chain / 截断；
  - `TestNewPlatform_QuotedEchoAndCardMentionOpts`：两个新 option 的解析与默认值；
  - `TestOnMessage_InteractiveCardMentionGate`：卡片在「默认免 @ / 开启后需 @ /
    开启后 @了仍放行」三种情形的行为。
  - `TestAckSessionFooter`：新话题首条 ack 隐藏临时 `om_` key，已有话题显示真实
    `omt_` key；对应 HybridGroup 端到端用例断言「新话题 ack 不含 `[session:`」。

验证命令：

```bash
go test ./agent/acp/ ./platform/feishu/
go build ./...
# 改动 core/engine.go、session.go 或命令处理时额外跑：
go test ./core/ -run TestCUJ
```

端到端人工验证：

1. 引用一张报警卡片 @Hermes → 话题内依次出现 ack、📎引用原文（链接齐全）、Agent 回复；
2. 观察 Agent 回复中不再夹杂思考过程；
3. （补完飞书权限后）直接转发卡片、不 @→ 自动处理；普通闲聊不 @→ 不响应。

---

## 9. 遗留事项

- [ ] **飞书后台给 Hermes 应用补 `im:message.group_msg` 权限并发版**——这是「转发卡片
      自动处理」唯一剩余动作，代码侧已全部就绪。
- [ ] 若希望 Hermes 也「全自动放行权限」，评估把 agent `mode` 从 `default` 改为
      `bypassPermissions`（ACP 无 `auto`）。
- [ ] 本次为本地 fork 改动（`agent/acp/mapping.go`、`platform/feishu/feishu.go`、
      `config.example.toml` + 测试），向上游同步前按 `SYNC-GUIDE.md` 流程走。
- [ ] 排障结束确认 `[log] level` 已回到 `info`；Hermes profile 的原生飞书保持 `enabled=false`。
