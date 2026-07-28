# CPA Multi-Protocol Provider

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 增加一个可自定义的上游提供商。它保留 OpenAI Compatibility 的主要配置能力，并允许为同一个提供商选择以下任一上游协议：

- OpenAI Chat Completions
- OpenAI Responses
- Anthropic Messages
- Gemini generateContent

插件支持自定义名称和 URL、模型别名、多个 API Key、每个 Key 的代理与优先级、图片模型端点、输入/输出模态以及思考参数。请求仍可从 CPA 已支持的任意客户端协议进入；CPA 在调用插件前转换请求，插件再通过 CPA 官方翻译层按客户端目标协议返回响应。

> CPA 的插件接口不能让第三方插件动态加入管理中心内置的“AI 提供商”列表。此插件会在“插件”菜单注册一个专用的“通用提供商”页面，提供完整的图形化配置体验；运行时模型和 Key 则接入 CPA 原生注册、调度、冷却与失败重试流程。

## 要求

- 官方原版 CLIProxyAPI v7.2.103 或更高版本
- 启用 CGO 的 CPA 构建（动态插件功能要求）
- 从源码构建时使用 Go 1.26.5 或更高的 1.26.x 安全补丁版本
- 预编译 Linux 包面向 GNU/Linux，最低 glibc 2.31

## 安装

### 自定义插件库（推荐）

本项目提供符合 CPA 插件商店规范的自定义库：

```text
https://raw.githubusercontent.com/Hamster-Prime/cpa-plugin-provider/main/registry.json
```

在 CPA 的 `config.yaml` 中，把该地址加入 `plugins.store-sources`：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/Hamster-Prime/cpa-plugin-provider/main/registry.json"
```

如果配置文件中已经存在 `plugins:`，只需把 `store-sources` 合并到现有节点，不要创建第二个 `plugins:`。自定义库会与 CPA 内置官方库同时显示，不会覆盖官方库。

CPA 需要能够访问 `raw.githubusercontent.com` 和 GitHub Release 下载地址（通常会跳转到 `release-assets.githubusercontent.com`）；网络受限时请先配置 CPA 的全局 `proxy-url`。这个自定义库使用 CPA 的 schema v2 直链安装格式，registry 已固定每个平台 ZIP 的 SHA-256 和大小，因此安装时不会查询 GitHub Release API，也不会触发匿名 API rate limit。全局 `plugins.enabled` 不会因安装动作自动开启，因此应保留上例中的 `enabled: true`。

1. 保存配置并重启 CPA，或使用现有的配置重载方式使其生效。
2. 打开 CPA 管理中心的“插件商店”并刷新列表。
3. 搜索 **Multi-Protocol Provider**，确认来源为上述自定义库，然后点击安装。CPA 会选择当前操作系统和架构对应的 Release ZIP，并使用 registry 中固定的 SHA-256 校验文件。
4. 如果 CPA 的全局插件开关尚未开启，按管理中心提示启用插件功能。
5. 安装完成后，在“插件”菜单打开“通用提供商”。
6. 首次打开时输入 CPA 的 Management Key（`remote-management.secret-key`）。该 Key 只保存在当前页面内存中，不写入浏览器存储。
7. 选择上游协议，填写 Base URL、模型和 API Key，然后保存并使用“测试连接”验证。

如果旧版 registry 已经返回过 GitHub API `403 rate limit exceeded`，请在本次 registry 更新后重启 CPA（或等待失败缓存过期）并刷新插件商店，然后重新安装；无需配置 GitHub Token。

“通用提供商”是插件的完整配置 GUI，可直接维护提供商名称、协议、Base URL、自定义请求头、模型与别名、图片模型、输入/输出模态、思考参数，以及带代理和优先级的多个 API Key。配置和凭据操作均通过 CPA 认证后的 Management API 完成，不需要手工编辑 YAML 或凭据文件。

后续更新和卸载也从 CPA 插件商店执行；自定义库会读取本仓库的最新正式 Release，不需要每次修改库地址。如果 CPA 提示需要重启，请完成重启后再继续使用；Windows 上更新已加载的动态库时通常必须重启。

### 手动安装（备选）

从 [GitHub Releases](https://github.com/Hamster-Prime/cpa-plugin-provider/releases) 下载当前平台的 zip 和 `checksums.txt`，先验证 SHA-256，再把 zip 根目录中的 `multi-protocol-provider.so`、`.dylib` 或 `.dll` 放入 CPA 配置的插件目录。Release zip 还包含 `README.md`、项目及第三方许可证和 `THIRD_PARTY_NOTICES.md`；CPA 插件商店只提取 zip 根目录中的动态库。

Linux 预编译包在 Debian Bullseye 环境使用 Go 1.26.5 生成，并在 CI 中检查最高 GLIBC 符号版本不超过 2.31。Alpine 等 musl 系统不能直接使用该包，需要在目标系统安装 Go、C 编译器和开发头文件后执行 `make build`，再安装使用相同 libc 和架构构建的动态库。

手动安装时可以先使用以下最小配置启用插件；启动 CPA 后，其余设置均可在“插件”菜单的“通用提供商”页面完成。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    multi-protocol-provider:
      enabled: true
```

需要完全以 YAML 管理时，可配置所有字段：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    multi-protocol-provider:
      enabled: true
      priority: 0
      name: My Provider
      protocol: openai-chat-completions
      base-url: https://api.example.com/v1
      prefix: example
      models:
        - name: upstream-model-id
          alias: model-id
          display-name: Example Model
          input-modalities: [text, image]
          output-modalities: [text]
          thinking:
            min: 0
            max: 32768
            zero-allowed: true
            dynamic-allowed: true
            levels: [low, medium, high]
        - name: upstream-image-model
          alias: image-model
          image: true
```

启动或重启 CPA 后，在“插件”菜单打开“通用提供商”。管理页保存时会先停用提供商并等待 CPA 完成重载，再写入 Key，最后以完整配置替换旧设置；在最终配置应用前失败时，提供商会保持停用，避免新 Key 被发送到旧 Base URL。页面会按 CPA 当前实际状态报告最终是否启用。修改协议或 Base URL 时，所有 API Key 以及带认证信息的代理 URL 都必须重新输入，管理接口也会在后端强制检查这一点。

## 从源码构建

```bash
make test
make build
```

## 协议与 URL

`base-url` 是协议版本根路径，必须是无用户名密码、query 和 fragment 的绝对 HTTP(S) URL。插件会在它后面追加对应端点：

| `protocol` | 请求端点 |
| --- | --- |
| `openai-chat-completions` | `/chat/completions` |
| `openai-responses` | `/responses`、`/responses/compact` |
| `anthropic-messages` | `/messages`、`/messages/count_tokens` |
| `gemini` | `/models/{model}:generateContent`、`streamGenerateContent`、`countTokens` |

例如 OpenAI/Anthropic 常用 `https://host.example/v1`，Gemini 常用 `https://host.example/v1beta`。插件不预设 Kimi 或任何厂商 URL。

图片模型通过 CPA 的 `/v1/images/generations` 与 `/v1/images/edits` 调用，始终使用 OpenAI Images 请求格式。图片开关不是单独的图片 URL。

未设置别名时，模型基础 ID 使用上游模型名。配置 `prefix` 后，CPA 默认同时注册基础 ID 和 `{prefix}/{alias}`；只有 CPA 全局开启 `force-model-prefix` 时才仅注册带前缀 ID。开启模型的“强制映射”后，响应中的模型名会还原为客户端请求使用的公开 ID。

## Key 存储

管理页通过 CPA 认证后的 Management API 将 Key 保存到 auth 目录的 `multi-protocol-provider.json`。一个文件会展开为多个 CPA auth 记录：

```json
{
  "type": "multi-protocol-provider",
  "destination": {
    "protocol": "openai-chat-completions",
    "base-url": "https://api.example.com/v1"
  },
  "keys": [
    {
      "id": "primary",
      "label": "Primary",
      "api-key": "secret",
      "proxy-url": "",
      "priority": 0,
      "disabled": false
    }
  ]
}
```

`destination` 将整个 Key 池绑定到保存时的协议和 Base URL。旧版未绑定的凭据文件，或绑定目标与当前配置不一致的文件，执行器都会返回 `503` 且不会发起上游请求；需要在管理页重新输入 Key 并保存后才能启用。这样即使保存最后一步的配置更新失败，刚输入的凭据也不会被后续请求发送到旧端点。

每个 Key 的 `id` 是稳定身份，增删或重排列表不会把另一条 Key 的冷却、统计或会话状态转移过来。管理状态接口只返回掩码，不回显明文 Key；运行日志和 auth metadata 也不会包含 Key。

每个 Key 可使用 `http://`、`https://`、`socks5://`、`socks5h://` 代理，也可填写 `direct` 或 `none`。管理接口会隐藏代理用户名和密码；“测试连接”和“获取模型”与正常模型请求使用同一套代理选择及鉴权逻辑。插件只跟随同源重定向，跨 scheme、host 或 port 的重定向不会携带 API Key 或自定义鉴权头继续请求。

## 开发验证

```bash
make fmt
make vet
make test
make race
make build
readelf -Ws dist/multi-protocol-provider.so | rg cliproxy_plugin_init
```

## License

[MIT](LICENSE)
