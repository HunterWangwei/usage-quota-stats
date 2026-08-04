# CLIProxyAPI 配置额度统计插件

这是一个适用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件：按凭据配置（`AuthID`）和模型统计输入、输出、缓存读写 Token、估算费用及缓存命中率，并在管理面板左侧增加“配置额度统计”页面。

## 从插件商店安装

在 `config.yaml` 添加插件源：

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

重启服务后打开插件商店，安装“配置额度统计”，再重启一次加载动态库。

## 在页面配置模型价格

进入左侧“配置额度统计”页面，在“模型价格”区域点击“添加模型”，填写模型名及每 100 万 Token 的输入、输出、缓存读取、缓存写入价格，然后点击“保存价格”。价格会保存到统计数据文件旁的 `.prices.json`，重启后仍然有效。

也可以在 `config.yaml` 中提供初始价格：

```yaml
plugins:
  configs:
    usage-quota-stats:
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

页面保存的价格优先于配置文件价格。精确模型名优先于前缀规则；多个前缀匹配时最长前缀优先。未定价模型仍会显示，但不计入费用合计。

缓存命中率公式为：`缓存读取 ÷（普通输入 + 缓存读取 + 缓存写入）`。插件只保存凭据标识、模型名、时间和 Token 计数，不保存提示词、回复、API Key 或 OAuth Token。统计记录保存在 `data-file` 指定的 JSONL 文件中。

## Docker 持久化

```yaml
volumes:
  - ./plugins:/app/plugins
  - ./data:/app/data
```

并将 `data-file` 设置为 `/app/data/usage-quota-stats.jsonl`。

## 手动构建

需要 Go 1.26+、CGO 和目标平台 C 编译器：

```powershell
./build.ps1
```

或：

```sh
./build.sh
```

构建结果位于 `dist/<goos>/<goarch>/`。更多英文说明见 [README_CN.md](README_CN.md)（文件名沿用历史版本）。
