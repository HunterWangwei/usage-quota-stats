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

进入左侧“配置额度统计”页面的“模型价格”区域：

- **待定价**：有用量但没有命中任何价格规则的模型会列在这里，按请求数排序。点击“设置价格”即可为它新建规则。
- **价格规则**：每条规则选择计费模式（按 Token / 按次），只显示该模式需要填写的价格。“命中模型”一栏列出实际使用了这条规则的模型和请求数，方便确认 `claude-*` 这类前缀规则覆盖了哪些模型。
- **导入 / 导出**：导出会下载当前规则的 JSON（同时复制到剪贴板）；导入可粘贴导出的 JSON，或 `config.yaml` 中的 `prices:` 配置段，同名规则被覆盖，其余保留。

修改后点击“保存价格”，页面不会刷新跳转。价格保存到统计数据文件旁的 `.prices.json`，重启后仍然有效。

也可以在 `config.yaml` 中提供初始价格：

```yaml
plugins:
  configs:
    usage-quota-stats:
      currency: CNY
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

金额默认按人民币（¥）显示，模型价格按 1 美元 = 1 元填写，例如官方 $3 / 百万 Token 直接填 3。

页面保存的价格优先于配置文件价格。精确模型名优先于前缀规则；多个前缀匹配时最长前缀优先。未定价模型仍会显示，但不计入费用合计。

## 账号成本与利润

在“账号成本与利润”页签：发现新账号时自动弹窗填写单号成本，同渠道同价格的账号自动记为一条进货记录，之后再进货不影响之前账号的成本；售价倍率按渠道（Antigravity、Gemini CLI、Codex、xAI）分别设置时间线，每笔请求按所属渠道在发生时的倍率计算收入。收入 = 等值费用 × 请求时倍率，利润 = 累计收入 − 账号成本，回本率 = 累计收入 ÷ 账号成本。成本和利润按全部历史计算，不受时间范围影响；设置保存在数据文件旁的 `.accounts.json`。

账号可以标记“下架”，利润定格在下架时刻。“数据与备份”页签：保存设置前自动备份旧版本（可一键恢复），每天自动完整备份，备份不会自动删除，也可以下载完整备份 zip。备份位于数据文件同目录下的 `usage-quota-stats-backups/`。




页面顶部可切换时间范围（今天 / 近 7 天 / 近 30 天 / 本月 / 全部），凭证卡片显示最近使用时间、失败率和状态，用量明细与账号利润都可以导出 CSV。按天统计默认用服务器时区，可通过 `timezone: Asia/Shanghai` 指定。


缓存命中率公式为：`缓存读取 ÷（普通输入 + 缓存读取 + 缓存写入）`。插件只保存凭据标识、模型名、时间和 Token 计数，失败请求只额外记录状态码，不保存提示词、回复、API Key 或 OAuth Token。统计记录保存在 `data-file` 指定的 JSONL 文件中。

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
