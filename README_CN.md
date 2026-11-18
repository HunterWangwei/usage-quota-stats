# CLIProxyAPI 配置额度统计插件

简体中文 | [English](README.md)

这是一个适用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生插件。它会按凭据配置和模型统计 Token 用量、估算费用，并在管理面板左侧菜单中增加独立的“配置额度统计”页面。

## 功能

- 按凭据配置（`AuthID`）和模型聚合请求，失败请求单独计数并显示失败率。
- 时间范围筛选（今天 / 近 7 天 / 近 30 天 / 本月 / 全部）和每日趋势图。
- 账号成本与利润：按进货批次设置单号成本和售价倍率，查看每个号、每批号的累计收入、利润和回本率。
- 凭证卡片显示最近使用时间和状态（正常、闲置、失败率高、不可用、已禁用）。
- 用量明细和账号利润均可导出 CSV。
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

推荐直接在“配置额度统计”页面的“模型价格”区域配置：

- **待定价**：有用量但没有命中任何价格规则的模型会列在这里，按请求数排序，点击“设置价格”即可新建规则。
- **价格规则**：每条规则选择计费模式（按 Token / 按次），只显示该模式需要填写的价格；“命中模型”一栏列出实际使用这条规则的模型和请求数。
- **导入 / 导出**：导出当前规则为 JSON；导入支持导出的 JSON 或下面 `config.yaml` 里的 `prices:` 配置段，同名规则被覆盖，其余保留。

页面保存的价格写入统计数据文件旁的 `.prices.json`，优先于配置文件。也可以在 `config.yaml` 中提供初始价格。

所有价格均为所选货币下每 100 万 Token 的价格。请将示例中的 `0` 替换成你的实际价格：

```yaml
plugins:
  configs:
    usage-quota-stats:
      enabled: true
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

字段说明：

| 字段 | 含义 |
| --- | --- |
| `currency` | 页面显示的货币，默认 `CNY`（显示为 ¥）。模型价格按 1 美元 = 1 元填写，不做汇率换算 |
| `input` | 普通输入 Token 价格 |
| `output` | 输出 Token 价格 |
| `cache-read` | 缓存读取 Token 价格 |
| `cache-write` | 缓存创建/写入 Token 价格 |
| `data-file` | 本地统计数据文件路径 |
| `timezone` | 按天统计使用的时区，例如 `Asia/Shanghai` |

精确模型名优先于前缀规则；多个前缀同时匹配时，最长前缀优先。没有匹配价格的模型不会计入费用合计，但仍会显示完整用量。

## 账号成本与利润

在“账号成本与利润”页签中：

- **收入** = 等值费用（按模型价格计算）× 售价倍率；**利润** = 累计收入 − 账号成本；**回本率** = 累计收入 ÷ 账号成本。
- 账号成本是一次性的买号价格，与模型价格使用同一种货币。成本和利润按账号的全部历史计算，不受顶部时间范围影响。
- 每批号建一个**进货批次**，填写单号成本和售价倍率。账号的成本和倍率来自它所在的批次，新建批次或调整默认批次不会改变已入批账号的利润；单个账号也可以单独填写覆盖批次。
- 每个渠道可以指定“新号默认批次”：账号第一次出现时自动归入当时的默认批次。账号表格支持多选后批量移入某个批次。
- 未分批的账号没有成本，按模型价格原价（倍率 1）计算收入。
- 金额按人民币显示，模型价格按 1 美元 = 1 元填写（官方 $3 填 3）。
- 从 1.1.0 升级时，原来的渠道默认值会自动转换成“××默认（1.1.0 迁移）”批次，利润数字保持不变。
- 设置保存在统计数据文件旁的 `.accounts.json`。

宿主已登记但还没有请求的账号也会列出，方便把闲置号的成本算进去。

## 时区

按天统计默认使用服务器时区。Docker 容器通常是 UTC，可以在配置中指定：

```yaml
plugins:
  configs:
    usage-quota-stats:
      timezone: Asia/Shanghai
```


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

插件只保存凭据标识、模型名、请求时间、Token 计数和失败请求的状态码，不保存提示词、模型回复、API Key 或 OAuth Access Token。支持权限控制的平台会以仅文件所有者可读写的权限创建数据文件。

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
