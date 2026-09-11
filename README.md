# workbuddy-cliproxy

把**腾讯 CodeBuddy**（`copilot.tencent.com`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA)插件,任何支持 OpenAI / Anthropic 协议的客户端(Claude Code、Cursor、Cline、SDK……)都能直接调用 CodeBuddy 背后的模型。

对 [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) 公开 `workbuddy.so` 的 clean-room 逆向重写,补齐了源码与 x86_64 支持;workbuddy 的原始设计归属 Sliverkiss。

## 工作原理

在 CPA 里注册为 `workbuddy` provider:负责 CodeBuddy 扫码登录、token 刷新,并把请求转发到
`copilot.tencent.com/v2/chat/completions`(国际版是 `www.workbuddy.ai`,路径完全相同)。

## 多账号

每个账号一份凭据文件 `workbuddy-<identity>.json`,`<identity>` 取账号 uid(拿不到时退化为
access token 的短哈希)。国内号和国际号可以并存,互不覆盖。

> 以前 `FileName` / `ID` 固定是 `workbuddy.json` / `workbuddy`,而 CPA 正是按 `FileName`
> 落盘的,所以第二个账号会直接覆盖第一个 —— 这就是"只能上传一个认证文件"的原因。
>
> **升级注意**:旧的 `workbuddy.json` 仍能被解析,但建议删掉后重新登录,避免和新的
> `workbuddy-<uid>.json` 重复。

## 国际版(Global)

国际版 `www.workbuddy.ai` 与国内版 `copilot.tencent.com` **接口路径完全一致**,只有域名不同
(已实测:`/v2/plugin/auth/state`、`/v2/chat/completions`、`/v3/config`、
`/console/enterprises/personal/models` 两边行为一致),所以支持国际版只是换个 base URL。

**但请求约束两边不一样,这是踩过的坑:**

| | 国内版 | 国际版 |
| --- | --- | --- |
| 非流式 `/v2/chat/completions` | 拒绝(code 11101) | 拒绝(code 11101) |
| 首条消息必须是 system | 不要求 | **要求**(否则 code 11128) |

非流式两个区都拒绝,插件统一改成向上游流式再聚合成一个 `chat.completion`
(`forceStreamBody`),这个早就处理了。首条消息必须是 system 只有国际版要求,由
`ensureSystemFirst` 处理:仅在 `region: global` 时补一条**空内容**的 system 消息
(实测空 system 就能通过校验,且不干扰模型输出)。国内版请求保持逐字节不变。

### 装成两个插件:一区一个(推荐)

宿主调 `auth.login.start` 时只给插件 `provider` 和 `baseURL`,**不传任何自定义入参**
(`Metadata` 永远是空的),而**一个插件只能注册一个 provider id**。所以单个插件的登录按钮
只能给一个区的链接,没法在界面上选区。

解法是**同一份代码编出两个 .so**,每区一个:

| 文件 | plugin id | provider | 区 | 上游 |
| --- | --- | --- | --- | --- |
| `workbuddy.so` | `workbuddy` | `workbuddy` | cn | `copilot.tencent.com` |
| `workbuddy-global.so` | `workbuddy-global` | `workbuddy-global` | global | `www.workbuddy.ai` |

依据(读 `CLIProxyAPI` 源码确认):

- `internal/pluginstore/install.go` `pluginIDFromPath`:**plugin id 就是文件名去掉扩展名**,
  且必须匹配 `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`。
- `internal/pluginhost/auth_provider.go` `ParseAuths`:provider 为空时按序问每个插件,
  **第一个返回 `handled` 的接管**;provider 非空时按 id **精确匹配**。
- `internal/watcher/synthesizer/file.go`:扫描凭据目录时用文件里的 `"type"` 字段当 provider
  —— 所以两个插件靠凭据文件的 `type` 天然分流,不会互相抢账号。

```bash
./build.sh          # 产出 workbuddy.so (cn) 和 workbuddy-global.so (global)
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
      priority: 100
      region: cn
    workbuddy-global:
      enabled: true
      priority: 100
      region: global
```

**区的选择靠 build tag,不靠 ldflags**:`providerName` / `buildRegion` 定义在
`provider_cn.go`(`//go:build !global`)和 `provider_global.go`(`//go:build global`)里,
`build.sh` 用 `-tags global` 选。试过 `-ldflags -X`,对 `-buildmode=c-shared` 的产物
**时灵时不灵**(字符串进了 buildinfo 但变量没被改写,表现为 `plugin_name` 仍是
`workbuddy`),build tag 是确定的。

面板上会看到两个 provider,各自一个登录按钮,点哪个就是哪个区 —— 不用改配置、不用重启。
两个插件同时装时,前面那个"一次登录给两个区 URL"的逻辑仍然保留为兜底(次要区的 URL 在
`metadata` 里),但已经不是主要路径了。

区域会写进每个账号的凭据文件(`"region":"global"`),之后该账号的刷新、模型发现、聊天请求
都固定走它自己的区。老凭据文件没有 `region` 字段,按 `cn` 处理,向后兼容。

> **迁移**:单插件时代建的 Global 账号,凭据文件叫 `workbuddy-<uid>.json` 且 `"type":
> "workbuddy"`。改成 `workbuddy-global-<uid>.json` + `"type": "workbuddy-global"` 就会被
> 国际版插件接管(CN 插件凭 `type` 精确匹配不会再抢它)。

## 模型

登录后按账号**动态发现**，不再硬编码:

1. `GET https://copilot.tencent.com/v3/config` —— 每个 CodeBuddy 客户端启动时拉的远程配置,
   `data.models` 就是该账号有权使用的模型目录(带 `maxInputTokens` / `maxOutputTokens` /
   `supportsImages` 等 serving 字段)。
2. 拿不到时退回旧接口 `GET /console/enterprises/personal/models`。
3. 都失败(网络不通 / token 失效 / 上游改结构)才用内置兜底列表:
   `default-model` · `auto-chat` · `glm-5v-turbo` · `kimi-k2.5` · `deepseek-v3.2` ·
   `gpt-5.5` · `gemini-3.5-flash`

结果按账号缓存 30 分钟。发现失败会走 `host.log` 打一条 `warn`,在 CPA 日志里能看到原因。

已用真实账号验证:带 Bearer 时 `/v3/config` 确实返回 `data.models`(实测每账号 29 个),字段同
`{id, name, vendor, descriptionZh/En, maxInputTokens, maxOutputTokens, supportsImages,
supportsToolCall, supportsReasoning, onlyReasoning, credits, ...}`。

目录里的内部模型会自动过滤掉:`completion-*` / `nes-*` / `enhance-*`(补全、next-edit
suggestion、提示词增强,不能走 chat completions)。

两个约束:

- `model.static` 返回空列表。模型目录在凭据后面,没有凭据就没有可用模型;如果这里再吐一份
  内置列表,`/v1/models` 会列出没有任何凭据能服务的模型,客户端一调用就得到
  `auth_not_found: no auth available`。所以目录只由 `model.for_auth` 提供,内置列表保留为
  `model.for_auth` 拉取失败时的兜底(此时它挂在该凭据下,是可路由的)。
- 上游给的字段缺失时才用默认值(上下文 200000、输出 8192)。

### 上游目录不全时:`extra_models`

`/v3/config` 并不总是完整。**国际版实测会漏掉整个 hy4 系列** —— 拿 OAuth token 直接调
`https://www.workbuddy.ai/v2/chat/completions`,`hy4-preview` / `hy4-preview-f` /
`hy4-preview-x` 全都返回 200(能用),但 `/v3/config` 里只有 `hy3`。而控制台那个
`/console/enterprises/personal/models` 确实列了它们,**但它只认浏览器 session cookie**,
带 Bearer 打过去在国际版是 500(国内版这个接口带 Bearer 是能通的),插件拿不到 cookie。

所以补一个显式配置项,避免把 id 硬编码进二进制:

```yaml
    workbuddy-global:
      enabled: true
      priority: 100
      region: global
      extra_models:
        - hy4-preview
        - hy4-preview-f
        - hy4-preview-x
```

规则:已在上游目录里的 id 不会重复添加(保留上游的真实元数据);`completion-` / `nes-` /
`enhance-` 这些内部模型配了也会被忽略。

顺带一个观察到的现象:**同一个 OAuth token 打国内域名 `copilot.tencent.com/v3/config` 也能
通**,而且返回 29 个模型、含 hy4 —— 但这个列表是按国内目录给的,里面有些 id(比如
`hy3-x`)在国际版上调不通(`11102 service info not found`),所以不能直接拿来当国际版目录用。

> **注意**:旧的硬编码列表(`hy3` / `hy3-preview` / `hy3-preview-agent` / `glm-5.2` /
> `glm-5.1` / `kimi-k2.7` / `minimax-m3-pay` / `deepseek-v4-*`)在当前目录里已全部不存在,
> 实测目录是 `default-model` / `auto-chat` / `glm-5v-turbo` / `kimi-k2.5` / `deepseek-v3.2` /
> `gpt-5.x` / `gemini-3.x` 这一批。兜底列表和 README 已同步更新。因此下面「思考模式」一节
> 里关于 hy3 的描述目前是死代码(账号若仍有 hy3 权限则照常生效)。

## 安装

**前置**:运行中的 CLIProxyAPI v7.2.x(带 CGO / 插件支持)、CodeBuddy 账号、Go 1.26+ 与 gcc;编译架构需与 CPA 实例一致(amd64 / arm64)。

```bash
git clone https://github.com/lovingfish/workbuddy-cliproxy.git
cd workbuddy-cliproxy
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o workbuddy.so .
```

产物:`.so`(Linux)/ `.dylib`(macOS)/ `.dll`(Windows)。放到 CPA 的 `plugins/` 目录,在 `config.yaml` 启用:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy: { enabled: true, priority: 100 }
```

重启 CPA,日志出现 `plugin loaded ... plugin_id=workbuddy` 即成功,`GET /v1/models` 也能看到上面的模型。然后到 CPA 面板添加 workbuddy 凭据,扫码登录 CodeBuddy。

## 使用

CPA 默认端口 `8317`,API key 见 `config.yaml` 的 `api-keys`。

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`(不带 `/v1`,走 `x-api-key`) |

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_API_KEY=<api-key>
export ANTHROPIC_MODEL=hy3-preview-agent
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"hy3-preview-agent","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

流式 / 非流式都支持;非流式请求会被内部转成流式再聚合(CodeBuddy 上游 `code 11101` 拒绝非流式)。

## Claude Code 兼容性

腾讯 CodeBuddy 的内容审核把 Claude Code 的两句固定 system 模板逐字加进了黑名单,命中即回"敏感内容"拒答:

- `You are Claude Code, Anthropic's official CLI for Claude.`(身份句)
- `Main branch (you will usually use this for PRs)`(git 注入句)

任何一字改动都绕过(精确匹配,非语义审核)。workbuddy 转发前会自动把这两句做最小改写(`CLI`→`CLI tool`、`Main branch`→`Default branch`),语义不变,Claude Code 照常工作。

属于 cat-and-mouse:腾讯哪天多加模板句,得跟着改 `sanitizeBlockedTemplates`。

## 思考模式

hy3 系列(`hy3` / `hy3-preview` / `hy3-preview-agent`)自动开最大思考:workbuddy 转发前强制 `reasoning_effort=high`,覆盖客户端任何设置。CodeBuddy 只对 `high` 真正开深度思考(`medium` / `max` / `xhigh` 等档位它直接忽略),所以这已是 hy3 能用的最高档。思考内容走 SSE 的 `delta.reasoning_content`,客户端要支持渲染思考块才看得到。

## 流式

真流式(async):转发上游时边读边通过 `host.stream.emit` 把每个 chunk 实时推给 CPA,客户端逐字收到(不是等收齐了一股脑)。hy3 几千字的思考过程也是实时流出的,不是憋半天再刷出来。

## License

MIT。
