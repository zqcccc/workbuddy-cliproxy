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
2. `GET /console/enterprises/personal/models` —— 控制台目录。**两个接口都拉,然后合并**,因为
   各自都列了对方没有的模型:`/v3/config` 有 `hy4-preview-f` 但没有 `hy4-preview-x`,
   控制台有 `hy4-preview-x` / `auto` 且是唯一带 `contextWindow` 可选预算的那个。
   谁都不是对方超集,只读一个会静默丢掉账号其实能用的模型。
3. 都失败(网络不通 / token 失效 / 上游改结构)时**不发任何模型**:返回空列表,
   等待下一次发现恢复。旧版本硬编码了一份"通用"兜底目录,但里面每一项要么在
   至少一个区按积分计费(glm-5v-turbo x0.71、kimi-k2.5 x0.45、gpt-5.5 x3.31、
   gemini-3.5-flash x0.99),要么两区目录都已不存在(deepseek-v3.2),上游对未知
   id 会静默路由到自己的默认后端再计费,所以已整体删除。

合并时同一 id 取**信息更全**的那条(带 `contextWindow` 的赢),顺序不敏感。

国际版注意:**控制台接口在国际版只认浏览器 session cookie,不认 Bearer**。

- 带 Bearer(插件运行时的认证方式) → 固定 **500**,换 UA / 换 header 组合都无效。
- 带浏览器 cookie(`session` + `session_2` 两个成对,缺一不可;UA 还得是
  `Chrome/152` 那一档) → **200**,实测 29648 字节、18 个模型。
- 国内版(`copilot.tencent.com`)宽容得多:**带 Bearer 直接 200**,无需 cookie。

插件 OAuth 登录走的是 `/v2/plugin/auth/state`(实测**不下发** session cookie,登录由用户在
浏览器完成),所以运行时手里只有 Bearer —— 国际版这条控制台接口因此拿不到。想让国际版也吃到
控制台数据,得把浏览器的 `session` / `session_2` 喂进来(会过期,需手动续),或者等上游把 hy4
加进 `/v3/config`。在此之前国际版 hy4 只能靠 `extra_models` 显式声明,见下节。

结果按账号缓存 30 分钟。发现失败会走 `host.log` 打一条 `warn`,在 CPA 日志里能看到原因。

已用真实账号验证:带 Bearer 时 `/v3/config` 确实返回 `data.models`(实测每账号 29 个),字段同
`{id, name, vendor, descriptionZh/En, maxInputTokens, maxOutputTokens, supportsImages,
supportsToolCall, supportsReasoning, onlyReasoning, credits, ...}`。

目录里的内部模型会自动过滤掉:`completion-*` / `nes-*` / `enhance-*`(补全、next-edit
suggestion、提示词增强,不能走 chat completions)。

两个约束:

- `model.static` 返回空列表。模型目录在凭据后面,没有凭据就没有可用模型;如果这里再吐一份
  内置列表,`/v1/models` 会列出没有任何凭据能服务的模型,客户端一调用就得到
  `auth_not_found: no auth available`。所以目录只由 `model.for_auth` 提供,而它在
  拉取失败时同样返回空列表,不再有内置兜底(见上文「模型」一节)。
- 上游给的字段缺失时才用默认值(上下文 200000、输出 8192)。

### 上下文窗口怎么算

部分模型(如 `hy4-preview`、`deepseek-v4.1-flash`)的目录里除了 `maxInputTokens` 还有一段可
选预算:

```json
"contextWindow": {"defaultLength": 200000, "supportedLengths": [200000, 1000000]},
"maxInputTokens": 1000000
```

`maxInputTokens` 是**硬上限**,`contextWindow.defaultLength` 才是这个账号**默认实际能用**的窗口
(CodeBuddy CLI 的 `resolveEffectiveContextBudget` 就是这么算压缩阈值的)。直接把硬上限当成上下文
报给下游,Claude Code / Cline 这类客户端会照着 100 万去装 prompt,结果上游按 20 万拒。

所以按 CLI 的同款逻辑解析:可选预算里命中 `defaultLength` 就用它,否则用最小的 `supportedLengths`,
只有一个可选值(等于没有预算)时退回 `maxInputTokens`,并且**永远不会超过 `maxInputTokens`**。
没有 `contextWindow` 的模型行为不变。

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

国际版目录同样会漏掉 deepseek 系列(`deepseek-v4.1-flash` 能调通但不在 `/v3/config` 里)。
这类"目录里查不到"的 id 建议用对象写法把上限写死,否则会落到 200000/8192 的兜底值
(见下节),cn2 上就是这么配的:

```yaml
      extra_models:
        - id: deepseek-v4.1-flash     # 上限取自国内版目录,国际版无接口可证
          name: Deepseek-V4.1-Flash
          maxInputTokens: 1000000
          maxOutputTokens: 128000
          maxAllowedSize: 1000000
          supportsImages: true
          supportsToolCall: true
          supportsReasoning: true
          onlyReasoning: true
```

规则:已在上游目录里的 id 不会重复添加(保留上游的真实元数据);`completion-` / `nes-` /
`enhance-` 这些内部模型配了也会被忽略。

#### 裸 id 的元数据从哪来:优先用接口,不是默认值

裸 id **不等于**用默认值。每次刷新目录时,插件会把裸 id 缺的字段用刚拉到的实时目录补上
(`fillFromCatalog`),所以接口怎么返回,对外就报什么 —— 上游哪天把 hy4 加进目录、或者改了
`maxOutputTokens`,下个 30 分钟刷新周期自动跟上,不用改配置。

只有**接口里也没有**这个 id 时,才退回默认值(上下文 200000、输出 8192),并且会在
`host.log` 里记一条 `defaultsFor: [...]`,方便你看出哪几个 id 没对上。

已用真实账号验证:

国内(控制台接口带 Bearer,200,30 个模型):

```
hy4-preview     ctx=1000000  out=64000   (接口值)
hy4-preview-x   ctx=1000000  out=64000   (接口值,经 extra_models 补进目录)
hy3             ctx=192000   out=64000
```

国际(浏览器 cookie,200,18 个模型):

```
hy4-preview   in=1000000  out=64000
              contextWindow: {defaultLength: 200000, supportedLengths: [200000, 1000000]}
hy3           in=192000   out=64000
```

注意国际版控制台目录**只列了 `hy4-preview`**,`-f` / `-x` 没有 —— 这两个的真实上限目前
**没有任何接口能证实**,只能按 `hy4-preview` 推断,推断值请显式写进配置而不要当实测。

对比改动前:hy4 全部报 `ctx=200000 / out=8192` —— 8192 是插件的兜底常量,不是上游限制。

#### 要写死某个值时:对象写法

想覆盖接口给的数字(比如上游标 64000 但你这账户实测撑不住),写成对象。显式写的字段**优先于**
接口值,不会被实时数据冲掉:

```yaml
      extra_models:
        - hy4-preview-f                 # 裸 id:跟随接口
        - id: hy4-preview-x             # 对象:只覆盖写出来的字段,其余仍跟随接口
          name: Hy4 preview
          maxOutputTokens: 32000        # 刻意压低
          contextWindow:
            defaultLength: 1000000
            supportedLengths: [200000, 1000000]
```

对象写法走的还是 `toModelInfos`,所以「上下文窗口怎么算」那套(`defaultLength` 优先、永不超过
`maxInputTokens`)对它一样生效。上面这个例子对外报 **1000000**;裸 id 跟接口走则报 200000
(因为接口给的 `defaultLength` 就是 200000)。**往上调之前先确认上游收得下**,否则客户端按 1M
装 prompt 会被拒。

顺带一个观察到的现象:**同一个 OAuth token 打国内域名 `copilot.tencent.com/v3/config` 也能
通**,而且返回 29 个模型、含 hy4 —— 但这个列表是按国内目录给的,里面有些 id(比如
`hy3-x`)在国际版上调不通(`11102 service info not found`),所以不能直接拿来当国际版目录用。

### 给某个区的模型加命名空间:`model_prefix`

国内/国际的目录有大量重名 id(`hy3`、`glm-5.x`、`kimi-k2.x`…),而 CPA **按 id 去重**,
重名的只会保留一条、归给 priority 高的那个。想显式指定"走国际版",用 `model_prefix`:

```yaml
    workbuddy-global:
      region: global
      model_prefix: global    # 目录里会多出 global/<model>
```

宿主会把它展开成 `<prefix>/<model>`(`sdk/cliproxy/service_models.go` `applyModelPrefixes`,
格式固定是 `前缀 + "/" + id`),选路时用 `rewriteModelForAuth` 把前缀剥掉再匹配 ——
所以 `global/kimi-k2.5` 一定落在国际版凭据上,而裸 `kimi-k2.5` 仍然按 priority 走国内。

**默认两种名字会同时存在**,即裸 id 也会注册到本插件名下(见下节为什么要避免)。
想让**只有** `<prefix>/<model>` 命中本插件,在 CPA 全局开:

```yaml
force-model-prefix: true
```

看起来是"全局"开关,其实**只对配了 `model_prefix` 的 provider 生效**,可以放心开:
`applyModelPrefixes` 第一行就是 `if trimmedPrefix == "" { return models }`,内置 provider
(codex / antigravity / gemini…)和没配前缀的插件根本没有 prefix,直接原样返回,不受影响。
(早期 README 说"会影响所有 provider"是错的,已订正。)

开启后实测:

| 请求 | 归属 |
|---|---|
| `global/hy4-preview-f` | workbuddy-global(国际版) |
| 裸 `hy4-preview-f` | 让给别的 provider(本例落到国内的 workbuddy) |

**注意宿主只剥前缀做"选路",不会改写转发给插件的请求体** —— 插件拿到的 `model` 还是
`global/kimi-k2.5`。所以插件自己要在发出去之前剥掉(`stripModelPrefixInBody`),否则上游
报 `11102 service info not found`。这一点是实测踩出来的。

加了前缀后还会顺带解放一些被别的 provider 占着的 id:`gpt-5.6-luna` / `gpt-5.6-terra`
原本归 openai provider,现在能以 `global/gpt-5.6-luna` 走国际版。

> ⚠️ **前缀是"加一份",不是"改名字"**,裸 id 依然会注册。而且
> `force-model-prefix: false`(默认)时宿主**会把裸 id 也匹配到 `<prefix>/<id>` 上**,
> 于是插件一旦发布 `global/gpt-5.6-luna`,裸 `gpt-5.6-luna` 的流量也会落到本插件 ——
> 再叠加插件 priority(100/200)高于内置 provider(默认 0),本插件反而排到 openai **前面**。
> 若国际版账号对该 id 没有权限,上游会挂起几十秒后返回空流,请求照发照计费。
> 要避免这种"抢注",见下一节。

### 只发布指定的模型:`publish_mode` / `publish_allow`

默认插件把上游目录整个发布出去。当目录里的 id **别的 provider 也在提供**时(典型:
`gpt-5.6-*` 归 openai、`claude-*` / `gemini-*` 归 antigravity),发布它就是把别人的流量
揽到自己账上。`publish_mode: allow` 让插件只发布白名单:

```yaml
    workbuddy-global:
      region: global
      model_prefix: global
      publish_mode: allow      # 只发 extra_models + publish_allow
      publish_allow:
        - hy3                  # 目录里确实属于本区、且账号能用的 id
      extra_models:
        - hy4-preview-f
        - id: deepseek-v4.1-flash
          maxInputTokens: 1000000
          maxOutputTokens: 128000
```

- `publish_mode: catalog`(默认)发全目录;`allow` 只发 `extra_models` + `publish_allow`。
- `publish_allow` 只是"从目录里挑",不会凭空造 id;目录里没有的写了也不出现。
- 目录拉取失败时**一律不发任何模型**(与 `publish_mode` 无关):插件暂时不报模型,
  等目录恢复。旧版本会回落一份内置兜底列表,那份列表里的 id 要么计费、要么两区
  目录都不存在,已删除。

验证方式(改完看有没有让出 id):

```bash
curl -s http://<cpa>/v1/models -H "Authorization: Bearer <key>" \
  | python3 -c "import sys,json,collections;d=json.load(sys.stdin)['data'];\
b=collections.defaultdict(list);[b[m['owned_by']].append(m['id']) for m in d];print(dict(b))"
```

### 上游 429 时冷却该凭证

上游限流(HTTP 429)时,插件会把这条凭证**冷却**一段时间,并且**不再对外报模型** ——
宿主因此把它从模型注册表里摘掉,同一个 model id 的请求就会落到**另一条还有这个模型的凭证**上,
而不是继续撞那条被限流的。

- 默认 60 秒;上游给了 `Retry-After` 就按它来;同一凭证反复失败按 2 倍递增,上限 30 分钟。
- 成功后立即解除冷却。
- **只有存在其他健康凭证时才隐藏模型**。如果所有已知凭证都在冷却,就继续报模型,
  让真实的 429 错误暴露出来 —— 否则用户只会看到"未知模型",更难排查。

注意一个边界:宿主一次只给插件一条凭证,插件没法自己"换一条",所以这里是靠
`model.for_auth` 返回空模型让宿主改选别的凭证。冷却期间不会打上游,不会加重限流。

> **注意**:旧的硬编码列表(`hy3` / `hy3-preview` / `hy3-preview-agent` / `glm-5.2` /
> `glm-5.1` / `kimi-k2.7` / `minimax-m3-pay` / `deepseek-v4-*`)在当前目录里已全部不存在。
> 后来硬编码的"通用兜底"(`glm-5v-turbo` / `kimi-k2.5` / `deepseek-v3.2` /
> `gpt-5.5` / `gemini-3.5-flash`)也已删除:每一项至少在一个区计费,三个两区都已不存在。
> 现在发现失败时直接返回空目录,不再发任何模型。因此下面「思考模式」一节里关于
> hy3 的描述目前是死代码(账号若仍有 hy3 权限则照常生效)。
>
> 两区目录里各有一条"自动选择"入口(国际版 `default-model`、国内版 `auto`,都带
> `isDefault`),它们是 App 里的菜单项而非模型:上游会自己挑后端,价格与能力都不可控。
> 插件不发布、不用它们兜底,见 `isRoutingAlias()`。注意国内版还有个 `default`
> (标价 x2.00、无 `isDefault`),那是**用户可主动选的正常模型**,照常发布和转发。

## 额度(剩余积分)

插件带一个 Management API 扩展,装好并重启后 CPA 面板左侧会出现额度菜单,
直接列出每个 workbuddy 账号还剩多少 credits、每个资源包的剩余/总量与周期截止时间,
每个包配一条进度条(≥70% 绿、≥30% 黄、<30% 红,和宿主内置 provider 的配色一致)。

菜单名**带区名**:国内插件是 **WorkBuddy 额度**,国际插件是 **WorkBuddy 国际版额度**。
两个插件是分开的两个 .so,如果都叫「额度」,面板里就是两条一模一样的菜单,
看不出哪个余额属于哪个区。页面标题与菜单名保持一致。

- 页面地址:`/v0/resource/plugins/<pluginID>/quota`,例如
  `http://<host>:8317/v0/resource/plugins/workbuddy/quota`
- 需要 JSON 时用鉴权路由:`/v0/management/workbuddy/quota`(Global 插件是
  `/v0/management/workbuddy-global/quota`),返回完整数据。
  加 `?refresh=1` 可跳过 60 秒缓存强制重查。
  注意这里要的是 **management key**(`remote-management.secret-key`),`api-keys` 里
  的 `sk-` 是给 `/v1` 用的,拿它访问会 401。
  两个插件的路由路径必须不同,否则宿主会按优先级丢弃其中一个。

页面数据来自两条上游接口(客户端里叫 api1 / api3),由插件用 `host.auth.list` /
`host.auth.get` 取回**自己名下**的凭据后逐个查询:

| 用途 | 接口 | 说明 |
| --- | --- | --- |
| 余额与档位 | `POST /billing/meter/get-user-resource-summary` | body 是 `{}`,返回 `Packages[]{PackageCode, CycleTotalCapacity, CycleRemainCapacity}` 与 `IsPaidUser` |
| 免费包与刷新周期 | `POST /billing/meter/get-user-resource-free-packages` | 必须带 `PackageCodes`,另带 `Status:[0,3]` 与当天 `SlicePeriodStartTime/EndTime` |

四个要点:

1. **额度是账户级积分池,不是每个模型一份。** 模型只有消耗倍率
   (`GET /v3/config` → `models[].credits`,如 `x0.00` ~ `x5.00`),`x0.00` 表示不扣积分
   (如 `hy3`)。单次实际扣费在 chat 响应的 `usage.credit`。
   **不存在"按模型的每日限量"**:查遍最新客户端也没有这个概念,每日刷新的是免费包的
   credits,不是模型维度。
2. **这两条路由在 API 根路径,没有 `/v2` 前缀。** `/v2/billing/meter/...` 一律 404
   (老的 `get-user-resource` 才有 `/v2`)。另外它们只接受 POST,且必须带
   `User-Agent: CLI/<ver> CodeBuddy/<ver>`,否则 403 `code 10085`(看着像权限问题,
   其实只是 UA 校验)。插件已经在 `commonHeaders()` 里处理。
3. **免费包会周期刷新。** `CapacityType == 4` 的包是"分片递减型",当期余量在
   `SlicePeriodUsageDetails[0]` 的 `SlicePeriod*Precise`。上游**并不对所有账号下发这个
   明细**(国内体验版实测就没有),缺它就只能显示整个周期的值,页面会标注"未下发"。
   所以代码里必须保留回落,不能硬取。
4. **面板查询不会刷新 token。** 上游 refresh 会轮换 refresh token,而这条路径不写回
   凭据,轮换后旧 token 就废了。所以过期账号只提示"请重新登录",不会自己去刷。

资源路由虽然按 CPA 的设计不走管理鉴权,但页面是从面板侧边栏打开的,属于管理页面,
账号名与 uid 直接完整展示,不做脱敏。

### 认证文件列表里的额度

面板「认证文件」每条凭据的名字后面也会带上余额,形如

```
WorkBuddy (小楚) · 剩 665.26 credits · CodeBuddy个人体验版 0/500
```

前半段是账户总余额,后半段是**会周期刷新的那个包**(`CapacityType == 4`)的余量——
赠送包只会越用越少,只有这个包会自己涨回来,所以把它单独带上,省得为了看一眼
免费额度跑去额度页。如果上游下发了当期切片(`SlicePeriodUsageDetails`),以切片为准。
认不出会刷新的包时就只显示总余额。

余额取的是上面 api1 的实时值。注意老接口 `get-user-resource` 的 `TotalDosage` 是
**陈旧快照**(实测同一个账号两处差 500),所以这里不用它。

这里有个宿主限制值得记一笔:CPA 的 auth-files 接口确实有 `quota` 字段,但
`coreauth.ProviderSupportsQuotaObservation()` 把它**硬编码给了 `claude` 和 `codex`
两个内置 provider**,外部插件填不进去,所以 Codex 那种"刷新额度"的展示方式对
workbuddy 用不了。能用的只剩凭据名字,但还藏着一个坑:**面板卡片的大标题渲染
的是 `email` 字段,不是 `label`**(`label` 完全不渲染,`account` 也不渲染,
email 为空才回落文件名)。Codex 卡看起来显示 label 只是因为它们的 label 恰好
等于邮箱。

`pluginapi.AuthData` 没有 Email 字段(只有 Label / Prefix / ProxyURL / Metadata /
Attributes 等),只能走 `internal/api/handlers/management/auth_files.go:602
authEmail()` 读的两个 map 之一:**`auth.Metadata["email"]` 和 `auth.Attributes["email"]`**。
这里**用 `Attributes`,不要用 `Metadata`** —— `pluginTokenStorage.SaveTokenToFile`
会用 `mergedStorageJSON(rawJSON, meta, provider)` 把 Metadata 合进落盘凭据,余额
写进去就变陈旧。Attributes 只在宿主内存侧,`sdk/auth/filestore.go:349` 对内建
provider 写的也是它。`main.go setAuthDisplay()` 在每次 parse / refresh 时把
label 同步写一份到 `Attributes["email"]`,AuthData.Label 和 Attributes["email"]
保持一致。

更新时机是宿主 `auth.parse`(重启、凭据文件变化)和 `auth.refresh`(token 续期),
两者都是"宿主把凭据交给插件、插件交还后由宿主落盘",没有并发写的问题。
**插件不会用 `host.auth.save` 回写**:那是整文件覆盖,和宿主自己的 refresh 抢写,
一旦覆盖到旧 token 就把凭据弄废了。


### 面板上的 WorkBuddy 图标

CPA 面板对内置 provider 的图标是**写死的表**(`claude` / `codex` / `gemini` /
`kimi` / `qwen` / `xai` / `vertex` / `meta` / `devin` / `iflow` / `aistudio`),
表里没有的 provider 一律回落成一个通用插头图形。插件唯一能自己控制的通道是
注册响应里的 `metadata.Logo`,面板会在**三处**读它:

- OAuth 页的 provider 卡片(`OAuthPage.tsx` 的 `buildPluginOAuthProviderCards`,
  取 `plugin.logo || plugin.metadata?.logo`)
- 插件管理页的插件卡
- 左侧边栏的插件条目

`logo.go` 把官方 WorkBuddy 徽标(40×40 圆角方形,青绿渐变 + 白色 W)以
**base64 data URL** 内联进去,原因有两条:面板跑在操作者浏览器里,不一定能访问
服务器能访问的域名,data URL 一定能解析;而且宿主会把 `metadata.Logo` 过一遍
`html.EscapeString`(见 `internal/api/handlers/management/plugins.go` 的
`entry.Logo = htmlsanitize.String(info.Metadata.Logo)`),裸内联 SVG 的 `<`
会被改写,base64 的字符集(`A-Za-z0-9+/=`)则原样通过。

**认证文件页的 provider 过滤标签走的是宿主前端的静态表,插件填不进。** 标签的图标
由 `AUTH_FILE_ICONS` 决定(键是凭据的 `type`,`ProviderTabs.tsx` 用它取
`<img src>`),表里没有 `workbuddy` 就回落成首字母方块;而且认证文件页不请求
插件列表接口,插件的 `logo` 传不到那里。仓库里的 `panel-workbuddy-icon.patch`
是给宿主前端 `Cli-Proxy-API-Management-Center` 打的补丁(基线 `4530da2`,
即 v1.24.2),补上这两个键(`workbuddy` / `workbuddy-global`)、同款 SVG 资产、
标签配色与四种语言的标签文案。打上之后**过滤标签、额度卡、OAuth 编辑器三处**
同时生效,因为三者共用同一个 `getAuthFileIcon`。凭据卡片(`AuthFileCard`)按
设计只渲染文字药丸、不渲染图标,上游还有测试断言它的源码不含 `<img>`,补丁
不动它。

重建与投放:

```bash
git clone --depth 1 https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git
cd Cli-Proxy-API-Management-Center
git apply /path/to/panel-workbuddy-icon.patch
bun install --frozen-lockfile && bun run build
# 产物 dist/index.html 改名为 management.html —— 宿主更新器只认这个名字
```

补丁只是源码,不会自己生效。要让 arm1 用上,得二选一:把
`remote-management.panel-github-repository` 指向放了 `management.html` 的
release 仓库,或者挂 `MANAGEMENT_STATIC_PATH` 并把
`remote-management.disable-auto-update-panel` 设为 `true` —— 只设路径不关
自动更新的话,宿主每隔 3 小时仍会拿上游 `/releases/latest` 覆盖掉本地文件。
改 compose 的 environment 或卷挂载后,`docker restart` 不会生效(它用容器创建
时的旧配置),必须 `docker compose up -d` 重建容器。

### 为什么卡片上做不了官方那种 CSS 进度条

Antigravity / Claude / Codex 卡片里那些「套餐 Pro → Five Hour Limit Remaining」
的进度条,是这么来的:

1. 宿主有个**通用的上游穿透接口** `POST /v0/management/api-call`
   (`internal/api/handlers/management/api_tools.go`),传 `auth_index` + `method` +
   `url` + `header` + `data`,header 支持 `$TOKEN$` 魔法变量(从
   `metadata.access_token` / `attributes.api_key` / `metadata.token` 取值),
   宿主拿该凭据的 token 代发请求。
2. 前端 bundle(`/CLIProxyAPI/static/management.html`)里**写死了 5 个 quota 配置**
   (antigravity / claude / codex / kimi / xai),每个带自己的 `filterFn` +
   `fetchQuota`。Antigravity 的 `fetchQuota` 就是拿 `api-call` 去打 Google 的
   `v1internal:retrieveUserQuotaSummary`。
3. 前端把返回的 `{groups, subscription, serverTimeOffsetMs}` 渲染成
   `quotaBarFill{High,Medium,Low}`(≥70 / ≥30 / <30 三档配色)。

**插件插不进去**:`sdk/pluginapi` 里 `quota` 零命中(既没 quota 字段也没 quota 方法),
前端那 5 个配置也不是后端下发的列表、没有 plugin 注册槽。所以卡片上做不了;
真·CSS 进度条只在我们自己的额度页上有(见上一节)。

另外 `ProviderSupportsQuotaObservation`(被动扫响应头那条路)只放 `claude`/`codex`,
而且有测试 `TestObserveResponseHeadersDropsKimiGrokAndAntigravitySignals` 明说
antigravity 的信号会被丢弃 —— 所以 antigravity 的条**不是**走被动观察,是走前端
主动 `fetchQuota` + `api-call`。

## 安装

**前置**:运行中的 CLIProxyAPI v7.2.x(带 CGO / 插件支持)、CodeBuddy 账号、Go 1.26+ 与 gcc;编译架构需与 CPA 实例一致(amd64 / arm64)。

```bash
git clone https://github.com/zqcccc/workbuddy-cliproxy.git
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

### 直接用预编译产物(不用本地编译)

GitHub Actions 会为每次构建产出各平台的 `.so`,发布在两个 tag 上:

| tag | 时机 | 内容 |
| --- | --- | --- |
| `rolling` | 每次 push `main` | 全平台,同一个 release 覆盖更新 |
| `v*` | 打了 `v` 开头的 tag | 全平台,每个 tag 各自留存 |

两个 tag 都发布成普通 release(不是预发布),产物直接出现在仓库 Releases 页面。
`rolling` 每次构建都会替换掉上一次的产物,只保留最新一次;需要留存历史版本就打 `v*` tag。

命名规则 `workbuddy-<goos>-<goarch>.so` / `workbuddy-global-<goos>-<goarch>.so`,
例如 Linux x86_64 取 `workbuddy-linux-amd64.so`。装到 CPA 的 `plugins/` 时**要改回**
`workbuddy.so` / `workbuddy-global.so` —— 宿主是按文件名认 plugin id 的。

```bash
# 例:Linux x86_64,取 rolling 最新构建
base=https://github.com/zqcccc/workbuddy-cliproxy/releases/download/rolling
curl -fL -o plugins/workbuddy.so        $base/workbuddy-linux-amd64.so
curl -fL -o plugins/workbuddy-global.so $base/workbuddy-global-linux-amd64.so
docker restart cli-proxy-api-plus   # 插件是启动时加载的,必须重启
```

## 自动部署(CI → 服务器)

`deploy/` 下是一个 webhook 接收端:CI 构建完直接把它签过名的产物清单 POST 到服务器,
服务器校验签名 → 下载本平台的 `.so` → 备份旧文件 → 覆盖 → 重启 CPA 容器。

两台服务器都装了同一套接收端,区别只在架构、插件目录和监听端口:

| 主机 | 架构 | 插件目录 | 接收端监听 | CI 是否投递 |
| --- | --- | --- | --- | --- |
| cn2 | x86_64 | `/root/code/cliproxyapiplus-docker/plugins` | `127.0.0.1:9000` | 否 |
| arm1 | aarch64 | `/home/ubuntu/code/cliproxyapiplus-docker/plugins` | `127.0.0.1:9001` | 是 |

arm1 上的 9000 被另一个无关的 webhook 服务占着,所以它的接收端放在 9001。

**CI 只投递 arm1。** cn2 的接收端还在跑,但收不到东西,它的插件不再随 push 自动更新;
要更新 cn2 得把 `DEPLOY_WEBHOOK_URL` 改回 cn2,或者手工把 amd64 产物拷过去重启容器。

```
push main / tag
      ↓
GitHub Actions: 各平台 build, 产物上传到 Release
      ↓
CI 用 HMAC-SHA256 签名, POST 到 DEPLOY_WEBHOOK_URL
      ↓
arm1  https://cliproxy-arm.onlylike.work/hooks/plugin-deploy
      ↓
校验 X-Hub-Signature-256 → 挑本平台 PLATFORM 的 .so → 备份 → 覆盖 → docker restart
```

这份 payload 里仍然列了全部四个产物(`workbuddy{,-global}-linux-{amd64,arm64}.so`),
接收端按自己的 `PLATFORM` 只装匹配的那两个,另一个架构的直接忽略。arm1 的 `PLATFORM`
是 `linux-arm64`,所以它装的是 arm64 那两个。以后要把 cn2 也接回来,只需要在 GitHub
再加一个 secret、CI 里多投一次,产物清单不用改。

CI 直接推而不用 GitHub 的 `release` 事件,是因为每次 push `main` 都是**更新**同一个
`rolling` release,GitHub 只会发 `edited` 而不会发 `published`,靠事件就只生效一次。

**一次性配置**:

1. 在 GitHub 仓库 Settings → Secrets 加两个变量(我没有写 secrets 的权限,这一步要手动):
   - `DEPLOY_WEBHOOK_URL` = `https://cliproxy-arm.onlylike.work/hooks/plugin-deploy`
   - `DEPLOY_WEBHOOK_SECRET` = 接收端安装时用的那个 secret(与机器上
     `/opt/plugin-deploy/plugin-deploy.env` 里的 `WEBHOOK_SECRET` 必须是同一个值,
     否则签名验不过)
2. 两台服务器上跑安装脚本(会生成 secret、装 systemd 单元):

```bash
ssh cn2 'bash -s' < deploy/setup-host.sh
ssh arm1 'PLATFORM=linux-arm64 PLUGIN_DIR=/home/ubuntu/code/cliproxyapiplus-docker/plugins LISTEN_PORT=9001 WEBHOOK_SECRET=<与接收端一致的 secret> bash -s' < deploy/setup-host.sh
```

3. 把脚本打印的 Caddy 片段加进各自机器的 `/etc/caddy/Caddyfile`,然后 `systemctl reload caddy`。
   两台机器的 site block 都要在兜底的 `reverse_proxy` 之前加上 `handle /hooks/plugin-deploy*`:
   cn2 转发到 `127.0.0.1:9000`,arm1 转发到 `127.0.0.1:9001`。

**日常运维**:

```bash
ssh arm1 journalctl -u plugin-deploy-webhook -f   # 看部署日志(CI 投递的那台)
ssh cn2  journalctl -u plugin-deploy-webhook -f   # cn2 接收端仍在跑,只是没人投
ssh arm1 python3 /opt/plugin-deploy/deploy-webhook.py status
ssh cn2  python3 /opt/plugin-deploy/deploy-webhook.py status
ssh arm1 python3 /opt/plugin-deploy/deploy-webhook.py rollback   # 列出可回滚的备份
ssh cn2  python3 /opt/plugin-deploy/deploy-webhook.py rollback
```

配置文件 `/opt/plugin-deploy/plugin-deploy.env`(`chmod 600`,里面有共享密钥),
两台机器各一份,`PLATFORM` / `PLUGIN_DIR` / `LISTEN_PORT` 按上面的表填。
改 `DRY_RUN=1` 可以只看会发生什么、不动线上文件。

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
