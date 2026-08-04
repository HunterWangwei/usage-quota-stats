# CLIProxyAPI 配置额度统计插件

简体中文 | [English](README.md)

这是一个适用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件。它会按凭据配置和模型统计 Token 用量、估算费用，并在管理面板左侧菜单中增加独立的“配置额度统计”页面。

## 功能

- 按凭据配置（`AuthID`）和模型聚合成功请求。
- 分别统计普通输入、输出、缓存读取和缓存写入 Token。
- 根据你填写的每 100 万 Token 价格计算预估费用。
- 支持精确模型名和 `claude-*` 形式的前缀价格。
- 缓存命中率计算公式：`缓存读取 /（普通输入 + 缓存读取 + 缓存写入）`。
- 自动处理 OpenAI 兼容接口与 Claude/Anthropic 不同的缓存 Token 计数口径，避免重复计费。
- 使用 JSON Lines 文件持久化，服务重启后统计不会丢失。
- 未配置价格的模型仍会显示，并明确标记为“未定价”。

## 从插件商店安装

在 CLIProxyAPI 的 `config.yaml` 中添加第三方插件源：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/HunterWangwei/usage-quota-stats/main/registry.json"
  configs:
    usage-quota-stats:
      enabled: true
```

重启 CLIProxyAPI，打开插件商店，安装“配置额度统计”。原生动态库安装完成后，再重启一次服务即可加载。

## 填写模型价格

所有价格均为所选货币下每 100 万 Token 的价格。请将示例中的 `0` 替换成你的实际价格：

```yaml
plugins:
  configs:
    usage-quota-stats:
      enabled: true
      currency: USD
      data-file: "usage-quota-stats.jsonl"
      prices:
        gpt-5.4:
          input: 0
          output: 0
          cache-read: 0
          cache-write: 0
        claude-*:
          input: 0
          output: 0
          cache-read: 0
          cache-write: 0
```

字段说明：

| 字段 | 含义 |
| --- | --- |
| `currency` | 页面显示的货币，例如 `USD`、`CNY` |
| `input` | 普通输入 Token 价格 |
| `output` | 输出 Token 价格 |
| `cache-read` | 缓存读取 Token 价格 |
| `cache-write` | 缓存创建/写入 Token 价格 |
| `data-file` | 本地统计数据文件路径 |

精确模型名优先于前缀规则；多个前缀同时匹配时，最长前缀优先。没有匹配价格的模型不会计入费用合计，但仍会显示完整用量。

## Docker 部署

建议同时持久化插件目录和统计数据目录：

```yaml
volumes:
  - ./plugins:/app/plugins
  - ./data:/app/data
```

并配置：

```yaml
plugins:
  configs:
    usage-quota-stats:
      data-file: "/app/data/usage-quota-stats.jsonl"
```

## 数据与隐私

插件只保存凭据标识、模型名、请求时间和 Token 计数，不保存提示词、模型回复、API Key 或 OAuth Access Token。支持权限控制的平台会以仅文件所有者可读写的权限创建数据文件。

## 手动构建

需要 Go 1.26+、CGO 和目标平台对应的 C 编译器。

Windows PowerShell：

```powershell
./build.ps1
```

Linux：

```sh
chmod +x build.sh
./build.sh
```

构建结果位于 `dist/<goos>/<goarch>/`。

## 许可证

[MIT](LICENSE)
