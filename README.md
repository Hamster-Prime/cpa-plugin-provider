# CPA Multi-Protocol Provider

为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 增加一个可自定义的上游提供商。它保留 OpenAI Compatibility 的主要配置能力，并允许为同一个提供商选择以下任一上游协议：

- OpenAI Chat Completions
- OpenAI Responses
- Anthropic Messages
- Gemini generateContent

插件支持自定义名称和 URL、模型别名、多个 API Key、每个 Key 的代理与优先级、图片模型端点、输入/输出模态以及思考参数。请求仍可从 CPA 已支持的任意客户端协议进入；CPA 会在调用插件前后完成协议转换。

> CPA v7.2.102 和 v7.2.103 的插件接口不能让第三方插件动态加入管理中心内置的“AI 提供商”列表。此插件通过 CPA 的插件管理菜单提供等价的专用管理页，运行时模型和 Key 则接入 CPA 原生注册、调度、冷却与失败重试流程。
>
> 使用 CPA v7.2.102 或 v7.2.103 时必须先应用仓库内的
> `patches/CLIProxyAPI-v7.2.102-plugin-host-security.patch`。补丁让宿主同步重解析多 Key 文件、以原子方式写入 `0600` 权限的凭据文件，并让动态插件复用所选 Key 的代理传输、应用 CPA 的思考参数解析、清理回调流，以及阻止携带鉴权头的请求跟随跨 origin 重定向。Unix 的 Go c-shared 模块和 Windows 的 Go DLL 一旦进入初始化阶段，都会撤销回调但保留进程内映射，避免热卸载导致运行时悬空；因此同一进程反复替换插件版本会保留少量旧模块内存，重启 CPA 后释放。未应用补丁时不应启用此插件。

## 要求

- CLIProxyAPI v7.2.102 或 v7.2.103 加仓库内的宿主安全补丁；更新版本需确认已经包含等价修复
- 启用 CGO 的 CPA 构建（动态插件功能要求）
- 从源码构建时使用 Go 1.26.5 或更高的 1.26.x 安全补丁版本
- 预编译 Linux 包面向 GNU/Linux，最低 glibc 2.31

## 构建与安装

```bash
make test
make build
```

GitHub Release 的每个平台 zip 都包含对应动态库、`README.md`、项目及第三方许可证、`THIRD_PARTY_NOTICES.md` 和 `patches/CLIProxyAPI-v7.2.102-plugin-host-security.patch`，Release 同时提供的 `checksums.txt` 可用于校验下载文件。Linux 预编译包在 Debian Bullseye 环境使用 Go 1.26.5 生成，并在 CI 中检查最高 GLIBC 符号版本不超过 2.31；Alpine 等 musl 系统不能直接使用该包，需要在目标系统安装 Go、C 编译器和开发头文件后执行 `make build`，同时使用相同 libc/架构重新构建已打补丁的 CPA。

在 CPA v7.2.102 或 v7.2.103 源码目录先检查并应用宿主补丁：

```bash
git apply --check /path/to/cpa-plugin-provider/patches/CLIProxyAPI-v7.2.102-plugin-host-security.patch
git apply /path/to/cpa-plugin-provider/patches/CLIProxyAPI-v7.2.102-plugin-host-security.patch
go test ./internal/pluginhost ./internal/thinking ./sdk/cliproxy/auth
```

然后重新构建 CPA。补丁以官方 `v7.2.102` 标签为基线，并已在官方 `v7.2.103` 标签上验证可干净应用及通过相关宿主测试；如果 `git apply --check` 失败，不要强行应用，应先核对当前 CPA 版本是否已经包含这些修复。

将 `dist/` 中生成的 `multi-protocol-provider.so`、`.dylib` 或 `.dll` 放入 CPA 的插件目录。

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

启动 CPA 后，在“插件”菜单打开“通用提供商”。管理页可维护以上配置和多个 Key；保存时会先停用提供商并等待 CPA 完成重载，再写入 Key，最后应用目标配置。中途任何步骤失败，提供商会保持停用，避免新 Key 被发送到旧 Base URL。修改协议或 Base URL 时，所有 API Key 以及带认证信息的代理 URL 都必须重新输入，管理接口也会在后端强制检查这一点。

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

每个 Key 可使用 `http://`、`https://`、`socks5://`、`socks5h://` 代理，也可填写 `direct` 或 `none`。管理接口会隐藏代理用户名和密码。管理页的“测试连接”不会绕过代理做直连：选择了每 Key 代理时会返回 `proxy_test_unsupported`，请保存后通过 CPA 的真实模型路由验证；正常模型请求会使用该代理。

宿主补丁允许同 origin 重定向，但拒绝跨 scheme、host 或 port 的重定向，避免 `X-Api-Key`、`X-Goog-Api-Key` 与自定义提供商请求头被发送到另一个站点。

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
