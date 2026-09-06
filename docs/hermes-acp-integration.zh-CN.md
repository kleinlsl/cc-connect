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
| 8 | 配了「自动放行」却还在弹授权；或引用回复时发 `/yolo`，Hermes 收到「前两轮对话 + /yolo」整段 | ① Hermes 只认 `default/accept_edits/dont_ask`，配 `auto/bypassPermissions/yolo` 会被**静默回退 default**；② `/yolo` 不是 cc 内置命令，未识别命令被当普通消息，连同引用回复的 Reply chain 一起透传 | 配置写 `mode="dont_ask"`；运行时用 `/mode dont_ask`（见 [2.3](#23-关于-mode与-claude-code-的关键区别)、[4.8](#48-yolo-被当普通消息引用链一起透传给-hermes)） |
| 9 | 重启后机器人「没反应」：进程在、TCP 长连在，但新消息全不进来，看到的是重启前的旧 ack | 重叠重启让飞书 WebSocket 进入「TCP 连着但不消费事件」的假活态，重启后 `message received` 为 0 | 确认无在跑子进程后再干净 `kickstart -k` 一次，确认单进程 + 一次 `projects=N`（见 [4.9](#49-重启后机器人没反应飞书-websocket-假活)） |
| 10 | 老会话发消息只有 ack、之后**所有会话**都不再 processing，CPU 0%，重启电脑才好 | cc 通用 ACP 死锁：Hermes `session/load` 全量重放历史 chunk，撑爆容量 128 的事件 channel，readLoop 阻塞 → load 响应读不到 → handshake 与事件消费互锁（与模型/上下文窗口无关） | 已修：handshake 完成前丢弃重放 update（见 [4.10](#410-resume-大会话时整个-engine-假死历史重放撑爆事件-channelcc-侧死锁-bug)） |
| 11 | 模型明明 `turn complete` 了，飞书却没收到回复（本机↔飞书网络抖动几分钟） | 旧逻辑同步重试只 3 次、约 3.5 秒就放弃，最终回复被永久丢弃，**无补发** | 两层兜底：①同步重试窗口拉到约 1 分钟；②新增持久化 **outbox 发件箱**，最终回复落盘、网络恢复/重启后自动补发；并补 immediateHeld 互斥 + 飞书 uuid 幂等，长任务不再把同一条回复发两遍（见 [4.11](#411-网络抖动导致模型已生成的回复丢失outbox-补发)） |
| 12 | 飞书对 Hermes 发 `/compact` 提示「不支持上下文压缩」；发 `/model` 提示「不支持模型切换」 | cc 的 `/compact`、`/model` 是内置命令，要求 agent 实现 `ContextCompressor` / `ModelSwitcher`，通用 ACP adapter 都没实现，命令在 core 被拦下、**到不了 Hermes**；而 Hermes 原生用的是 `/compress`、`/model`（走 session/prompt 文本通道） | ACP 新增可配置 `compress_command` / `model_command`，把 cc 的 `/compact`、`/model` **透传**成 Hermes 原生斜杠命令并把回执发回飞书；`/model <名>` 当场切当前会话、保留历史；自定义网关必须三段式 `custom:<provider>:<model>`（见 [4.12](#412-acphermes-的上下文压缩与模型切换斜杠命令透传)） |
| 13 | Hermes(ACP) 回复底部没有 Claude 那样的 `model · out/in · ctx%` + 工作目录状态行 | 通用 ACP adapter 没实现 `GetContextUsage()`、也没消费 `usage_update`/prompt `usage`/握手 `models.currentModelId`；且 core 两行状态行原本要求必须有 Claude 的 cw/cr 信号才渲染 | ACP 上报用量与模型名，core 放宽为「有 cache 或有逐轮 in/out」即渲染，无 cw/cr 时省略该段，Claude 呈现零变化（见 [4.13](#413-hermesacp-回复底部没有状态行model--token--ctx--工作目录)） |

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
      mode = "default"                # Hermes 仅认 default/accept_edits/dont_ask（见 2.3）；自动放行编辑用 "dont_ask"
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

### 2.3 关于 `mode`（与 Claude Code 的关键区别）

cc 的通用 ACP adapter **只透传** mode 字符串（`session/set_mode`）、不做翻译，所以**合法取值
完全由具体 agent 决定**，不能照搬 claudecode 的值。

Hermes 的合法值以其源码 `acp_adapter/server.py` 的 `_MODES` 为准，**只有 3 个**：

| mode | 编辑审批策略 | 含义 |
|---|---|---|
| `default` | ask | 每次文件编辑前都请求授权（默认） |
| `accept_edits` | workspace_session | 工作区与 `/tmp` 内编辑自动放行，敏感路径仍问 |
| `dont_ask` | session | 本会话文件编辑一律自动放行，仅敏感路径仍问 |

注意：

- **没有 `auto`、`yolo`、`plan`、`bypassPermissions`，也不是驼峰 `acceptEdits`**。`auto` 是
  claudecode 专用值，`bypassPermissions` 是通用 ACP / 其他 agent 的值——对 Hermes 都无效。
- Hermes 的 `set_session_mode` 收到**不认识的 mode 会静默回退到 `default`（不报错）**，所以配错
  的典型表象就是「明明配了自动放行，却还在弹授权」。要让「编辑别再问」，配 **`mode = "dont_ask"`**。
- 运行时切换用 `/mode`（不带参数会列出 Hermes **实际上报**的模式并给按钮）或 `/mode dont_ask`。
  **`/yolo` 不是 cc-connect 的内置命令**（见 [4.8](#48-yolo-被当普通消息引用链一起透传给-hermes)）。
- 边界：Hermes 的 mode 只覆盖**文件编辑审批**；shell 等其他需要授权的工具不受它控制。

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

### 4.8 `/yolo` 被当普通消息、引用链一起透传给 Hermes

#### 现象
在群里**引用回复**并输入 `/yolo` 想「全部放行」，结果 Hermes 收到的是：

```
--- Reply chain (2 messages) ---
[1] User (user): <被引用的报警卡片全文>
[2] User (user): @机器人
---
/yolo
```

前两轮对话被一起带进 prompt，`/yolo` 没有起到切换权限的作用。

#### 根因（两层叠加）
1. **`/yolo` 不是 cc-connect 的内置斜杠命令**：命令表 `builtinCommands`（`core/engine.go`）里
   只有 `/mode`，没有 yolo。`handleCommand` 对未识别命令返回 false，于是 fall-through 当成普通
   消息发给 agent。代码注释里俗称的「/yolo」只是口头说法；真正的「全部放行」批准词是**权限
   卡片弹出时**回复「允许所有 / allow all」（词表 `approveAllPhrases`）。
2. **引用回复会把父消息链拼在正文前**：平台层把当前输入放 `Content`、被引用链放 `ExtraContent`
   （`platform/feishu/feishu.go` 的 `formatReplyChain`），engine 再合成
   `msg.Content = ExtraContent + "\n" + content`（`core/engine.go` 约 2887 行）。因为第 1 步没把
   `/yolo` 拦成命令，整段（含两条引用）就一起进了 Hermes。

#### 正确用法
- 切换权限模式：`/mode`（无参数列出当前 agent **实际**支持的模式、带按钮）或 `/mode dont_ask`。
- 权限确认卡片弹出时：回复「允许所有 / allow all」= 本会话 cc 层全部放行（approveAll）。
- 想持久自动放行文件编辑：配置里直接写 `mode = "dont_ask"`（见 2.3），之后无需手动切。

#### 附：后台任务的权限请求为何被「自动拒绝」
当前台这一轮（user turn）已结束、Hermes 又在**后台**发起工具授权（如 turn 完成后继续写文件）时，
没有人能点确认卡片，cc 会**默认拒绝并发提示**：
「⚠️ 后台任务请求使用工具 … 但已自动拒绝（当前无活跃会话）」。对应 `core/engine.go` 中
`EventPermissionRequest` 的 unsolicited 分支，i18n key 为 `MsgBackgroundAutoDenied`。让 Hermes
不再发起编辑授权（`mode="dont_ask"`）、或在权限卡片回「允许所有」置位 approveAll 后，这类后台
请求会被自动 allow，不再提示。

### 4.9 重启后机器人「没反应」：飞书 WebSocket 假活

#### 现象
`launchctl kickstart -k` 时旧进程优雅退出慢（在清理多个 agent 子进程），与 KeepAlive 拉起的
新进程**两轮重叠**。之后：主进程在、到飞书的 TCP 长连也是 ESTABLISHED，但重启后一条
`message received` 都没有；用户看到的 ack 其实是重启前旧进程发的，对应 Hermes 处理已被重启
杀掉（日志 `acp: session/new: context canceled`），表象就是「acp 失败、Hermes 没反应」。

#### 定位
- 重启后长时间无任何 `message received`，日志文件停止增长；
- `lsof -nP -iTCP -sTCP:ESTABLISHED -p <pid>` 仍能看到飞书 443 长连（**TCP 活 ≠ 事件面活**）；
- 管理面 `bridge:` 连接照常，说明进程没死，只是飞书事件没被消费。

#### 解法
确认没有在跑的 agent 子进程后，**再干净 `kickstart -k` 一次**（这次无慢退出、不会重叠），确认
只剩 1 个进程且只出现一次 `cc-connect is running projects=N`；随后群里发条消息，应立刻看到
`message received → session spawned → turn complete`。

### 4.10 resume 大会话时整个 engine 假死：历史重放撑爆事件 channel（cc 侧死锁 bug）

#### 现象
重启 cc 后，在一个**有较长历史**的 Hermes 话题/私聊里发消息：收到 ack「收到，正在处理中…」
后再无下文；更糟的是此后**任何群、任何会话**的新消息都只有 `message received`、连
`processing` 都没有，整个 engine 像被冻住。进程都在、CPU 0%、飞书长连也正常，重启电脑才恢复。
全新会话偶尔正常，越是老会话越必现。

#### 根因（cc 通用 ACP adapter 的自死锁，与模型/上下文窗口无关）
1. Hermes 的 `session/load` 在返回响应前，会把该会话**整段历史重放**成 `session/update`
   通知（实测 240 条消息的会话重放出 444 个 chunk：user/agent/thought/tool_call/tool_call_update）。
2. cc 的 `StartSession` 在主 goroutine **同步**执行 handshake、等 `session/load` 的正式响应；
   而 readLoop goroutine 同时在读这些重放 chunk，每读一条就 `emit` 进**容量仅 128** 的
   `events` channel。
3. `events` 的消费方 engine 要等 `StartSession`（handshake）返回后才开始 `Events()` 取事件。
   于是重放超过 128 条后 channel 被填满，`emit` 阻塞 → readLoop 停住 → `session/load` 的正式
   响应永远读不到 → handshake 永久等待 → engine 永不消费 → **环形死锁**，并卡住 engine 上
   后续所有消息。全新 `session/new` 不重放历史（0 chunk）所以能过；Claude 走专用 adapter、
   不这样重放，因此一直正常。

#### 解法（已修，`agent/acp/session.go`）
加 `handshakeDone` 标志：handshake（initialize + session/new|load）**完成前**，readLoop 收到的
`session/update` 一律视为历史重放/初始化噪声——只更新内部缓存（mode、toolInput），**不 emit**
到事件 channel；handshake 成功返回后才放行实时 update。这样重放再多也不会反压 readLoop，
顺带避免把历史 thought/tool/消息重发到 IM。回归测试：
`agent/acp/session_load_test.go::TestHandshake_ResumeLoadReplayDoesNotDeadlock`
（修复前 5s 超时失败、修复后通过）。

#### 快速验证
修好的二进制部署后，在**原来卡死的老会话**里发一条：日志应在数秒内走完
`message received → processing → (session/load 重放被静默吞掉) → acp: live mode applied →
turn complete`，不再停在 processing；且其他会话也能正常处理。

### 4.11 网络抖动导致「模型已生成的回复」丢失（outbox 补发）

#### 现象
Hermes 日志已经 `turn complete`、回复文本都生成了，但飞书侧迟迟收不到；cc 日志最后是
`platform send failed: reply failed after 3 retries ... dial tcp 220.181.175.91:443:
connect: connection refused`。几分钟后网络自行恢复，但那条回复**永远不会再发**。

#### 根因（两层都不够）
1. 飞书适配器内有同步瞬时重试 `withTransientRetry`，但旧参数只重试 **3 次**、退避
   0.5s→1s→2s，**总窗口约 3.5 秒**；本次本机↔飞书断了几分钟，救不回来。
2. 同步重试耗尽后，engine 只打一条 `platform send failed` 就把错误丢弃（`send()` 里
   `_ =` 忽略），**全项目没有任何出站消息持久化/补发机制**，最终回复就此丢失。

> 注意和「模型慢」区分：模型 prefill 慢时 Hermes 端还没 `turn complete`，属于上游推理/缓存
> 问题（见第 9 节遗留）；本节是**回复已生成、只卡在最后一跳发送**。

#### 解法：两层兜底（默认开启，对所有走 outbox 能力的平台生效）

**第一层 P0 —— 拉长同步重试窗口。** `platform/feishu` 的
`maxTransientRetries 3→6`、`transientRetryMaxDelay 5s→30s`，退避
0.5→1→2→4→8→16→30s，总窗口约 1 分钟，覆盖绝大多数秒级抖动。

**第二层 P1 —— 持久化 outbox 发件箱（`core/outbox.go`）。**
- engine 在发**最终回复**前，先把「平台名 + 会话 key + 编码后的 replyCtx + 已渲染正文/页脚」
  原子落盘到 `<data_dir>/outbox/*.json`（复用 `AtomicWriteFile`），然后分块发送；每发成功
  一块就把进度（`sent_chunks`）落盘，全部成功才删文件。
- 任一块发送失败：错误经平台实现的 `SendErrorClassifier.IsRetryableSendError` 判断——
  网络类（refused/timeout/reset/EOF/5xx，飞书复用 `isTransientError`）**保留待补发**；
  永久错误（bot 被移出群、参数错、无权限）立即丢弃不空转。
- 后台 replayer 按指数退避（5s 起、上限 5min、带抖动）补发，**只补没发出去的块、绝不重跑
  模型**；收到任意新入站消息、或某次发送成功（说明网络恢复）会立即 `Kick` 提前补发，不必
  干等定时器。
- **进程重启也能恢复**：启动时扫描 outbox 目录，未完成的回复继续补发。
- 兜底：超过 `max_age`（默认 30 分钟）或 `max_attempts`（默认 12 次）转 dead、删文件并
  `slog.Error` 告警，避免几小时后的「幽灵回复」。
- **只补最终回复**：ack、思考、工具进度等过程消息不进 outbox（补发只会刷屏）。
- **防重复：互斥 + 幂等双保险（把 at-least-once 补成「不丢且不重」）。** outbox 本质是「至少送达一次」，
  早期只靠「Add 后 30s 初始 grace」这个时间窗避免即时发送与补发线程撞车；Hermes 长达数分钟的 turn
  叠加慢发送会跨过该窗口，同一条最终回复被即时路径和 replayer 各发一次（飞书收到两条重复）。两道闸：
  - **进程内互斥 `immediateHeld`**：`Add` 即由即时发送路径持有该 item，replayer 的 `sweep` 直接跳过
    被持有项，与发送耗时彻底解耦（不再赌时间窗）；只有即时路径判定为可重试错误、调 `Fail` 时才释放并
    交接给 replayer，成功则 `Complete` 删除。持有态只在内存（`json:"-"`），**进程崩溃重启后自动释放**，
    恢复补发不受影响。
  - **平台幂等（飞书请求体 `uuid`）**：每条最终回复在 `Add` 时生成一个 RFC4122 uuid 并随 item 持久化，
    经 context（`core.WithOutboxUUID` / `OutboxUUIDFromContext`）透传到飞书 `Reply/Create` 请求体的
    `uuid` 字段；即时发送与之后所有重发共用**同一个** uuid，飞书对相同 uuid 1 小时内至多成功一次，是
    互斥之外的最终兜底（即便重启或竞态漏网，平台侧也只落一条）。非 outbox 发送的 ctx 不带 uuid，行为不变。
  - **优雅关停不与补发竞态**：`Engine.Stop` 先冻结 session 落盘（`SessionManager.StopPersistence`）、等待在途消息处理 goroutine 收敛（`turnWg`），最后才停止 outbox replayer（`Outbox.Stop`）；关停后不再写 sessions/outbox 数据文件，避免重启补发与临时目录清理相互竞态。

平台通过两个**可选接口**按需启用（core 不硬编码平台名，符合架构约束）：
`ReplyContextCodec`（replyCtx 序列化/反序列化，飞书已实现）、
`SendErrorClassifier`（发送错误是否可重试，飞书已实现）。未实现的平台保持旧的
fire-and-forget 行为。

#### 配置（顶层 `[outbox]`，默认即推荐值，通常无需写）
```toml
[outbox]
enabled = true       # 默认 true，关掉则退回旧行为
max_age_mins = 30    # 超过 30 分钟未发出则丢弃
max_attempts = 12    # 单条最多尝试 12 次
```

#### 验证
- 单测：`core/outbox_test.go`（补发成功、重启恢复、保序、TTL/次数上限转 dead、禁用空转、
  Run+Kick 集成、`ImmediateHoldBlocksSweep` 持有期 sweep 不抢、`ImmediateFailReleasesHoldForReplay` 失败交接、`HoldReleasedAndUUIDPersistedAcrossRestart` 重启释放持有且 uuid 留存、`UUIDUniquePerReply`、ctx 往返）；`platform/feishu/outbox_codec_test.go`（replyCtx 往返、错误分类）。
- CUJ：`core/cuj_test.go::TestCUJ_G7_OutboxRedeliversAfterOutage`，用户视角三步——
  断网发消息（看不到、但落盘）→ 恢复网络（无需重问，自动补发）→ 再发一条（即时达、无积压）。
- 线上：制造断网时日志出现 `final reply delivery interrupted, queued for redelivery`；
  恢复后出现 `outbox: final reply redelivered`，飞书收到回复；`~/.cc-connect/outbox/`
  正常情况下应为空。

---

### 4.12 ACP/Hermes 的上下文压缩与模型切换（斜杠命令透传）

**现象**：飞书里对 Hermes 发 `/compact` 回「当前 Agent 不支持上下文压缩」；发 `/model`
回「当前 Agent 不支持模型切换」。

**根因**：`/compact`、`/model` 都是 cc-connect 的**内置命令**，core 在转发给 agent 之前先要求
agent 实现对应能力接口——压缩要 `ContextCompressor`、模型切换要 `ModelSwitcher`；通用 ACP
adapter 两个都没实现，于是命令在 core 直接被「不支持」拦下，**根本到不了 Hermes**。而 Hermes
其实原生支持这两件事，只是命令名/形态不同：压缩是 `/compress`（不是 `/compact`），模型切换是
`/model [name]`，二者都走普通 `session/prompt` 文本通道，回执也是纯文本。

**解法（配置驱动的命令透传，默认关闭、向后兼容）**：

- core 侧用两个**可选接口**表达「该 agent 用原生斜杠命令完成某能力」：
  - `ContextCompressor.CompressCommand()`（已有）返回如 `/compress`；
  - `ModelCommand.ModelCommand()`（本次新增）返回如 `/model`。
- 通用 ACP adapter 不写死任何厂商命令名，分别从 agent options `compress_command` /
  `model_command` 读取（TrimSpace，空=不支持，维持旧行为）。
- `/compact` 与 `/model` 都复用同一套「取活跃会话 → 会话锁 → 向 `session/prompt` 发送命令 →
  收事件并把文本回执发回 IM」骨架（`runForwardedCommand` / `processForwardedEvents`），
  压缩的自动触发路径行为不变。
- `cmdModel` 的判定顺序：**先**看是否实现 `ModelCommand`（透传，ACP/Hermes 走这条）；否则回退
  结构化 `ModelSwitcher`（Claude/OpenCode 等走这条，逻辑不变）；都没有才回「不支持」。

**Hermes 侧行为（已核对 `acp_adapter/commands.py`）**：

| 在飞书发 | 透传给 Hermes | Hermes 回执 / 效果 |
|---|---|---|
| `/compact` | `/compress` | 原地把 `state.history` 压成摘要，**不换 ACP 会话**，回 `Context compressed: a -> b ...` |
| `/model` | `/model` | 回 `Current model: X / Provider: Y` |
| `/model custom:new-api-01:z-ai/glm-5.3-flash` | 原样透传 | **当场**重建该会话 agent、保留全部历史、不换会话，回 `Model switched to: ...`（自定义网关必须三段式，见下） |

> 注意与 cc 原生 `ModelSwitcher` 的语义差别：原生切换是「改**下次**会话默认 + 本地持久化」，
> 还要求 `AvailableModels` 拉列表渲染按钮；ACP 是长驻进程、Hermes 要的是「当场切当前会话」，
> 且 Hermes `/model` 不返回可选列表，所以**不采用**「让 ACP 实现完整 ModelSwitcher」的方案。
> 代价：透传模式下 `/model` 没有点按钮选模型的卡片，需要自己知道模型名（无参可查当前）。

**自定义网关（new-api 等）必须用三段式，裸模型名会掉回 openrouter 报错**：

Hermes 解析 `/model` 入参用的是**冒号 `:`** 分段（不是斜杠）。当 provider 是配置里的自定义
网关（如 `new-api-01` → `http://localhost:3003/v1`）时，必须写成 `custom:<provider配置key>:<模型id>`：

| 在飞书发 | 结果 |
|---|---|
| `/model custom:new-api-01:z-ai/glm-5.3-flash` | ✅ provider=`custom:new-api-01`，base_url 仍是 :3003/v1，带 key，正常切换 |
| `/model z-ai/glm-5.3-flash`（裸模型名） | ❌ 沿用 current 时重建 agent 丢 base_url，`resolve_runtime_provider` 兜底到 openrouter 且无 key → `No LLM provider configured` |
| `/model new-api-01:z-ai/glm-5.2`（缺 `custom:` 前缀） | ❌ `new-api-01` 非内置知名 provider，冒号不拆，同样失败 |

- 模型 id 里的斜杠（`z-ai/glm-5.3-flash`）保留，只有**段间**用冒号。
- `/model` **没有** `--help`/`list` 子命令（发 `--help` 会被当成模型名）；用 `/help` 看全部命令，
  无参 `/model` 查当前模型。必须先说话拉起会话，无活跃会话时命令只回提示。
- 不要给模型名加 `[1M]` 之类后缀：网关分组下没有该渠道会返回 503 `model_not_found`。
- 网关当前可用模型可直接查：`curl http://localhost:3003/v1/models`（key 从 profile config /
  `~/.claude/settings.json` 读，勿外泄）。

**给某个会话换模型的三种方式（语义不同，别混用）**：

1. 改 profile 默认 `~/.hermes/profiles/<p>/config.yaml` 的 `model.default`：**只对之后新建会话
   生效**；老会话恢复时读 state.db 里该会话自己存的 model，不跟随（重启 cc 也不变）。
2. IM 里 `/model <名>`（本次能力，需配 `model_command`）：**当前话题当场切、历史全留**，首选。
3. 兜底：停掉 cc（launchd 是 KeepAlive，要用 `launchctl bootout` 而非 `stop`）→ 终端
   `hermes -p <p> --resume <session-uuid>` 进去 `/model`，或直接改 `state.db` 的 `sessions.model`
   → 再 `bootstrap`+`kickstart`。**必须先停 cc**，否则 cc 内存里的旧 agent 下一轮会把改动覆盖回去。

---

### 4.13 Hermes(ACP) 回复底部没有状态行（model · token · ctx% + 工作目录）

**现象**：Claude Code 项目每条回复底部有两行
`mimo-v2.5-pro · out 1.0k · in 1.0k cw 0 cr 36.7k · ctx 19%` + 工作目录；Hermes(ACP) 项目什么都没有。
这不是开关问题——`show_context_indicator` 默认就是 true，它只决定「有数据时显不显示」，根因是 ACP
adapter 压根没上报数据。

**根因（两处能力缺口）**：

1. `agent/acp` 没实现 `ContextUsageReporter.GetContextUsage()`，也没消费 Hermes 的用量信号；
   `session/prompt` 响应被直接忽略，只 emit 了 `EventResult`。
2. core 的两行状态行渲染（方法版 `buildClaudeStatusLineFooter`）原本写了两道硬门槛：没有 context
   window、或没有 Claude 的 prompt-cache 信号（cw/cr）就直接返回空、回退到 legacy 单行 footer。
   cw/cr 是 Anthropic 专有，Hermes/OpenAI 系天然为 0，于是永远进不来。

**ACP 协议里的用量从哪来（已核对 Hermes `acp_adapter/server.py` 与 SDK `acp/schema.py`）**：

- `session/update` 的 `usage_update` 通知：`update = {sessionUpdate:"usage_update", size(上下文窗口总量),
  used(当前已用)}`；Hermes 在 `session/new|load` 后会立即推一次，之后按压力推。
- `session/prompt` 响应里的 `usage`（camelCase）：`inputTokens / outputTokens / totalTokens /
  thoughtTokens(=reasoning) / cachedReadTokens / cachedWriteTokens`。
- 当前模型名在**握手响应**里就有：`session/new|load` 返回的 `models.currentModelId`，无需额外 RPC。
  注意 `/model` 切换后 Hermes 不会主动推新模型名（`session_info_update` 只含标题），属已知小限制——
  切换后状态行模型名可能仍是握手时的旧值，下次新会话刷新。

**实现**：

- `agent/acp/session.go`：session 结构加 `lastUsage *core.ContextUsage`（`usageMu` 保护）与
  `currentModel`（`modelMu`）；握手解析 `models.currentModelId`；`onNotification` 吸收 `usage_update`
  的 size/used；每次 `session/prompt` 响应吸收 per-turn usage；新增 `GetContextUsage()`（锁、克隆、
  nil 安全）与 `GetModel()`，并加编译期接口断言。两个来源是**合并**关系：usage_update 管窗口/已用，
  prompt usage 管每轮 in/out，互不覆盖。注意口径：Hermes/OpenAI 系 `inputTokens` 是**整段请求**、
  `cachedReadTokens` 是其中命中缓存的**子集**（日志 `cache=命中/总量(命中率)`，见 `agent/turn_usage.py`），
  不能像 Anthropic 那样把 input 与 cache 相加；缺 usage_update 时 used 回退为 `InputTokens` 本身，不叠加 cached。
- `core/engine.go` 方法版 `buildClaudeStatusLineFooter`：删掉「必须有 cw/cr」的硬门槛，改为
  **有 cache 信号（cw/cr 非 0）或有逐轮 in/out token** 才走两行状态行；都没有（只有窗口占用 +
  账号额度桶）仍回退 legacy 单行。token 段：有 cache 时保持 Claude 原样 `in X cw Y cr Z`（零值也显示），
  无 cache 时只显示 `in X`；`out`/`ctx%` 按值非零才出现。

**呈现差异（刻意为之）**：

| agent | 状态行 line1 |
|---|---|
| Claude Code | `model · out N · in N cw N cr N · ctx N%`（行为零变化） |
| Hermes / 通用 ACP | `model · out N · in N · ctx N%`（没有 cw/cr 段） |

line2 工作目录逻辑不变；`reply_footer` / `show_context_indicator` / `show_workdir_indicator`
三个开关对 ACP 同样生效。

**验证**：单测 `agent/acp/session_usage_test.go`（两种 usage 合并、clone 隔离、握手模型）、
`core/claude_status_footer_test.go`（无 cache 渲染简化行、仅窗口占用回退 legacy、Claude 全字段不变）；
端到端需在飞书给 hermes 项目发消息，看回复底部两行。

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
| `ack_show_session_key` | `true` | 为 `false` 时，ack「收到，正在处理中…」后不再附带 `[session: …]`（按 project 单独配置） |

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

### 6.3 agent options（`[projects.agent.options]`）的 `mode`

Hermes（`type="acp"`）的 `mode` 只接受 `default` / `accept_edits` / `dont_ask`（详见 2.3）：

```toml
[projects.agent.options]
  mode = "dont_ask"   # 文件编辑自动放行（仅敏感路径仍问）；配错值会被 Hermes 静默回退 default
```

> 该 mode 是**新会话初始值**（StartSession 时经 `session/set_mode` 下发）；改配置后需重启
> cc-connect，或在 IM 里 `/mode dont_ask` 对当前会话实时生效。

### 6.4 顶层 `[outbox]`（最终回复失败补发，默认开启）

```toml
[outbox]
enabled = true       # 默认 true；false 退回「发失败即丢」的旧行为
max_age_mins = 30    # 未发出的回复最长保留 30 分钟，超时丢弃并告警
max_attempts = 12    # 单条最多尝试 12 次
```

> 落盘位置 `<data_dir>/outbox/*.json`（data_dir 默认 `~/.cc-connect`）。只兜底**最终回复**，
> 过程消息不补；补发只重发已生成内容、不重跑模型。机制详见 [4.11](#411-网络抖动导致模型已生成的回复丢失outbox-补发)。

### 6.5 agent options 的斜杠命令透传（`compress_command` / `model_command`）

仅 `type="acp"` 且后端确实支持相应无头斜杠命令时配置；**留空=该能力回退「不支持」**，因此对
其他 ACP server（Copilot 等）零影响。Hermes 两个都配：

```toml
[projects.agent.options]
  compress_command = "/compress"   # cc 的 /compact 转发成它（Hermes 用 /compress，不是 /compact）
  model_command = "/model"         # cc 的 /model 转发成它；/model <名> 当场切当前会话、保留历史
```

> 机制与三种换模型方式见 [4.12](#412-acphermes-的上下文压缩与模型切换斜杠命令透传)。

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
- 斜杠命令透传（压缩 + 模型切换，见 4.12）：
  - `agent/acp/agent_test.go`：`TestNew_ModelCommandDefault`（默认空且实现接口）、
    `TestNew_ModelCommandCustom`（配置值 TrimSpace），并在 `TestWorkspaceAgentOptions`
    断言 `model_command` round-trip；
  - `core/engine_test.go`：`TestCmdModel_ModelCommand_NoSession_RepliesNoSession`、
    `TestCmdModel_ModelCommand_ForwardsAndRelays`（断言发给会话的就是 `/model <args>` 且
    Hermes 回执被转发回 IM）、`TestCmdModel_NoSwitcher_RepliesNotSupported`（回归：两条路都
    不实现时仍回「不支持」，不能被透传分支吞掉）。

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
- [x] Hermes「自动放行文件编辑」已落地：agent `mode = "dont_ask"`（Hermes 仅认
      `default/accept_edits/dont_ask`；`auto`/`bypassPermissions` 对它无效、会静默回退 default，见 2.3）。
- [ ] 本次为本地 fork 改动（`agent/acp/mapping.go`、`platform/feishu/feishu.go`、
      `config.example.toml` + 测试），向上游同步前按 `SYNC-GUIDE.md` 流程走。
- [ ] 排障结束确认 `[log] level` 已回到 `info`；Hermes profile 的原生飞书保持 `enabled=false`。
