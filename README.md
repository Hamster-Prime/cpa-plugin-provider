# CPA Multi-Protocol Provider

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 增加一个可自定义的上游提供商。它保留 OpenAI Compatibility 的主要配置能力，并允许为同一个提供商选择以下任一上游协议：

- OpenAI Chat Completions
- OpenAI Responses
- Anthropic Messages
- Gemini generateContent

插件支持自定义名称和 URL、模型别名、多个 API Key、每个 Key 的代理与优先级、图片模型端点、输入/输出模态以及思考参数。请求仍可从 CPA 已支持的任意客户端协议进入；CPA 在调用插件前转换请求，插件再通过 CPA 官方翻译层按客户端目标协议返回响应。

> CPA 的插件接口本身不能让第三方插件动态加入管理中心内置的“AI 提供商”列表。本项目同时提供[配套的管理中心分支](https://github.com/Hamster-Prime/Cli-Proxy-API-Management-Center)，它会把此插件显示为“AI 提供商”中的独立“通用协议”分类；使用官方管理中心时，完整 GUI 仍可从单独的插件页面打开。运行时模型和 Key 均接入 CPA 原生注册、调度、冷却与失败重试流程。

## 要求

- 官方原版 CLIProxyAPI v7.2.103 或更高版本
- 启用 CGO 的 CPA 构建（动态插件功能要求）
- 从源码构建时使用 Go 1.26.5 或更高的 1.26.x 安全补丁版本
- 预编译 Linux 包面向 GNU/Linux，最低 glibc 2.31

Linux ARM64 必须使用 CPA 的普通 `linux_aarch64` 发布包；名称带 `_no-plugin` 的包是 CGO=0 便携版，官方明确不支持动态插件。OpenWrt/musl 环境也不能直接使用这个预编译插件，需要在兼容的 glibc 环境运行 CPA，或同时为 CPA 和插件准备带 CGO 的同环境构建。

## 安装

### 自定义管理面板 + 自定义插件库（推荐）

本项目提供符合 CPA 插件商店规范的自定义库：

```text
https://raw.githubusercontent.com/Hamster-Prime/cpa-plugin-provider/main/registry.json
```

在 CPA 的 `config.yaml` 中同时选择配套管理面板仓库，并把插件库地址加入 `plugins.store-sources`：

```yaml
remote-management:
  panel-github-repository: "https://github.com/Hamster-Prime/Cli-Proxy-API-Management-Center"

plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/Hamster-Prime/cpa-plugin-provider/main/registry.json"
```

如果配置文件中已经存在 `remote-management:` 或 `plugins:`，请把字段合并到现有节点，不要创建第二个同名节点。自定义库会与 CPA 内置官方库同时显示，不会覆盖官方库。配套管理面板只改变 Web UI，不替换 CPA 主程序。

CPA 需要能够访问 `raw.githubusercontent.com` 和 GitHub Release 下载地址（通常会跳转到 `release-assets.githubusercontent.com`）；网络受限时请先配置 CPA 的全局 `proxy-url`。这个自定义库使用 CPA 的 schema v2 直链安装格式，registry 已固定每个平台 ZIP 的 SHA-256 和大小，因此安装时不会查询 GitHub Release API，也不会触发匿名 API rate limit。全局 `plugins.enabled` 不会因安装动作自动开启，因此应保留上例中的 `enabled: true`。

1. 保存配置并重启 CPA，使插件宿主和管理面板仓库配置同时生效。
2. 打开 CPA 管理中心的“插件商店”并刷新列表。
3. 搜索 **Multi-Protocol Provider**，确认来源为上述自定义库，然后点击安装。CPA 会选择当前操作系统和架构对应的 Release ZIP，并使用 registry 中固定的 SHA-256 校验文件。
4. 安装后重启 CPA，再刷新管理中心。
5. 打开“AI 提供商”，选择新增的“通用协议”分类。
6. 配套面板会把当前已登录的 Management Key 通过严格来源校验传给插件 iframe；使用官方面板的独立插件页面时，才需要在页面内再次输入 Management Key。插件页面不会把该 Key 写入浏览器存储。
7. 选择上游协议，填写 Base URL、模型和 API Key，然后保存并使用“测试连接”验证。

CPA 下载管理面板时会访问 GitHub Releases API。若日志中出现 API rate limit `403`，普通安装请从[配套面板 Releases](https://github.com/Hamster-Prime/Cli-Proxy-API-Management-Center/releases)下载 `management.html`，放入 CPA 配置文件同级的 `static/management.html`，然后重启 CPA。`GITSTORE_*` 环境变量会同时启用或改变 CPA 的 Git 凭据存储后端，不要仅为了面板下载而设置；只有原本就在使用正确 GitHub GitStore 仓库的部署，才应复用其现有 Token。

如果旧版 registry 已经返回过 GitHub API `403 rate limit exceeded`，请在本次 registry 更新后重启 CPA（或等待失败缓存过期）并刷新插件商店，然后重新安装；无需配置 GitHub Token。

CPA 内置官方库在刷新其它插件的版本信息时仍可能访问 GitHub API；如果希望同时提高这些请求的配额，可以把 Token 放在环境变量中，再添加可选配置（不要把 Token 写进 YAML 或 registry）：

```yaml
plugins:
  store-auth:
    - match: "https://api.github.com/"
      apply-to: ["metadata"]
      type: github-token
      token-env: "CPA_PLUGIN_GITHUB_TOKEN"
```

然后在启动 CPA 的环境中设置 `CPA_PLUGIN_GITHUB_TOKEN`。本项目的 direct registry 安装路径本身不需要这个 Token。

“通用提供商”是插件的完整配置 GUI，可直接维护提供商名称、协议、Base URL、自定义请求头、模型与别名、图片模型、输入/输出模态、思考参数，以及带代理和优先级的多个 API Key。配置和凭据操作均通过 CPA 认证后的 Management API 完成，不需要手工编辑 YAML 或凭据文件。

后续更新和卸载也从 CPA 插件商店执行；自定义库会读取本仓库的最新正式 Release，不需要每次修改库地址。如果 CPA 提示需要重启，请完成重启后再继续使用；Windows 上更新已加载的动态库时通常必须重启。

### 安装后显示“未注册”

“已安装”只代表 ZIP 已下载并写入插件目录；“已注册”还要求 CPA 宿主成功加载动态库。请依次检查：

1. `CLIProxyAPI --version` 至少为 v7.2.103。
2. Linux ARM64 使用的是 `CLIProxyAPI_<version>_linux_aarch64.tar.gz`，不是 `_no-plugin.tar.gz`。
3. CPA 的管理接口响应头 `X-CPA-SUPPORT-PLUGIN` 为 `1`；为 `0` 表示当前 CPA 是 CGO=0 构建，无法加载标准动态插件。
4. 重启 CPA 后查看日志中的 `pluginhost` 行。如果出现 `GLIBC_* not found`、`wrong ELF class` 或 `cannot open shared object`，说明 libc 或架构不匹配。

插件注册成功后，管理中心需要重新打开或刷新页面。配套管理面板会在“AI 提供商”中显示“通用协议”；官方管理面板只会显示独立插件页面。

### 手动安装（备选）

从 [GitHub Releases](https://github.com/Hamster-Prime/cpa-plugin-provider/releases) 下载当前平台的 zip 和 `checksums.txt`，先验证 SHA-256，再把 zip 根目录中的 `multi-protocol-provider.so`、`.dylib` 或 `.dll` 放入 CPA 配置的插件目录。Release zip 还包含 `README.md`、项目及第三方许可证和 `THIRD_PARTY_NOTICES.md`；CPA 插件商店只提取 zip 根目录中的动态库。

Linux 预编译包在 Debian Bullseye 环境使用 Go 1.26.5 生成，并在 CI 中检查最高 GLIBC 符号版本不超过 2.31。Alpine 等 musl 系统不能直接使用该包，需要在目标系统安装 Go、C 编译器和开发头文件后执行 `make build`，再安装使用相同 libc 和架构构建的动态库。

手动安装时可以先使用以下最小配置启用插件；启动 CPA 后，其余设置可在配套管理面板的“AI 提供商 > 通用协议”中完成，使用官方管理面板时则从独立插件页面完成。

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

启动或重启 CPA 后，在配套管理面板的“AI 提供商”中打开“通用协议”；使用官方管理面板时从独立插件页面打开“通用提供商”。管理页保存时会先停用提供商并等待 CPA 完成重载，再写入 Key，最后以完整配置替换旧设置；在最终配置应用前失败时，提供商会保持停用，避免新 Key 被发送到旧 Base URL。页面会按 CPA 当前实际状态报告最终是否启用。修改协议或 Base URL 时，所有 API Key 以及带认证信息的代理 URL 都必须重新输入，管理接口也会在后端强制检查这一点。

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
