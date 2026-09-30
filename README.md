# fuck-openai

**GPT 订阅毫无透明度：你每月付 $200，换来的是一个没有分母的百分比。**
额度砍了多少、每个模型扣得多快，OpenAI 不告诉你。这个工具读 Codex 写在你电脑上的日志，把每一次请求按 OpenAI API 官方价折成美元，
再对照服务器返回的额度百分比，**实时算出你这周的额度到底值多少钱**。额度被砍，这个数会直接掉下来。

*Local tool that prices every Codex request (desktop app, VS Code, CLI) at OpenAI API rates and back-solves what your
weekly ChatGPT-plan quota is actually worth in dollars — so a silent quota cut shows up as a number.*

---

## GPT 订阅毫无透明度

- **你能看到的只有一个百分比。** Codex 的用量条只告诉你「已用 33%」，从来不告诉你 100% 是多少。
- **官方定价页对个人套餐只给宽得没法用的区间。** [Codex 定价页](https://learn.chatgpt.com/docs/pricing)写 Plus 用 GPT-6 Astra
  「每 5 小时大约 5–45 条消息」——上下相差 9 倍；不给 token 数，也不写 Pro 是 Plus 的几倍。
- **唯一写明「每个模型每百万 token 扣多少」的费率卡，只适用于企业客户。**
  [ChatGPT Rate Card](https://help.openai.com/en/articles/20001106-codex-rate-card) 标题就写着
  「Business, Enterprise/Edu credit-based pricing」。个人 Plus / Pro 用户扣额度的速率，没有任何公开文档。

结果就是：额度有没有变、换个模型是不是更费，个人用户完全无从核对。

## 额度正在被砍

OpenAI 帮助中心 [About ChatGPT Pro tiers](https://help.openai.com/en/articles/9793128-about-chatgpt-pro-tiers) 原文：

> If your Pro 200 subscription was active at the eligibility cutoff or during the seven days before it, you're eligible
> to keep your previous included usage allowance through Oct 29, 2026 while you have an active Pro 200 subscription.
> After that date, your subscription will move to the lower included usage allowance. Your subscription price stays at $200/month.

**低多少？官方原文没写。** 媒体报道（[The Next Web，2026-09-29](https://thenextweb.com/news/openai-devday-pro-200-usage-cut-pro-500-plan)）
说 Pro 200 在 Codex 里的额度从 Plus 的 20 倍降到 10 倍，也就是减半。价格不变，用量条还是那一个百分比——10 月 29 日之后，你的
100% 只剩原来的一半，界面上看不出任何区别。

## 作者实测：同样 1% 的额度，换个模型能差一倍多

如果个人订阅真按费率卡的比例扣（费率卡上每个模型的点数恰好都是 API 价 × 25），那用哪个模型烧满一周，折成 API 价都应该一样。
实测不是这样：

| 模型 | 每 1% 周额度消耗的未缓存输入 token | 整周额度折合 API 价值 | 同样 $1 API 价值扣的额度（Astra = 1） | 测量日期 |
|---|---|---|---|---|
| gpt-6-astra | 1,277,404 | $1,277 | 1.00 | 2026-09-30 |
| gpt-6-sol | 6,496,007 | $1,299 | 0.98 | 2026-09-30 |
| gpt-5.6-sol | 4,445,160 – 4,615,339 | $1,778 – 1,846 | **0.70** | 2026-09-30 |
| gpt-5.6-terra | 5,768,746 | $1,152 – 1,154 | 1.11 | 2026-09-11、09-29 |
| gpt-5.6-luna | 55,595,505 | $1,112 – 1,126 | 1.14 | 2026-09-11、09-29 |
| **gpt-6.1-sol**（9 月新上） | 4,914,857 | $983 | **1.30** | 2026-09-30 |
| **gpt-6-luna** | 约 77,580,000 | $776（误差 $751–863） | **1.65** | 2026-09-30 |

- 同一个 100%，按 API 价值从 **$776 到 $1,846**，差 2.4 倍。新上的 gpt-6.1-sol 比同等 API 价值的 Astra **多扣 30%**，gpt-6-luna
  多扣 65%。这些都没有写在任何官方文档里。
- 测法：一个老 Pro 200（20 倍）账号；每次只发一种模型、每发约 5 万 token 的未缓存输入、串行；测量期间账号上的其他请求已核对剔除；
  记录百分比每跳 1% 之间烧掉的 token 数，每个模型至少两个独立读数（含前一天的复测）。Luna 这种便宜模型烧一格要几千万 token，改用混合法：先用 Astra 把读数卡在刚跳格的位置，
  再烧目标模型 N 个 token，最后用 Astra 补烧到下一格，由补烧量解出它的权重。
- 缓存命中的输入：测过的模型都按自己输入价的 10% 扣（gpt-6.1-sol 是 5%），和 API 价目表里「缓存价 ÷ 输入价」的比例一致。
- 顺带澄清一个传言：**订阅里超长上下文（单次输入 > 272K）不会加倍扣额度。** 每发约 9.4 万 token 和每发约 35 万 token 对照，
  gpt-6-astra、gpt-6-sol、gpt-5.6-sol 每 1% 消耗的 token 数在误差范围内相同（API 按量计费时，超过 272K 的请求是按 2 倍收的）。

一个账号的实测只是一个样本。**用这个工具测你自己的账号**，10 月 29 日前后各看一次，砍了多少你自己心里有数。

## 这个工具做什么

### 1. 周额度实时价值

```bash
python3 codex_cost.py quota              # 本周额度值多少钱 + 历史各周
python3 codex_cost.py quota --watch 60   # 每 60 秒刷新一次
```

Codex 每次请求返回时，都会把服务器当时的「周额度已用百分比」写进本地日志。工具把一个周窗口里的请求按 API 价折成美元，
除以这段时间百分比涨了多少，就得出**整周额度折合多少 API 美元**。本周的窗口还会告诉你剩下的百分比大约还值多少。

作者本机的真实输出（节选 4 周、省略了「费率卡点」一列；老 Pro 200，主要用 gpt-5.6-sol）：

```
重置时间         套餐   已用% 起→止   这段API价值$   整周额度≈$     取整误差范围$   主力模型
08-02 03:18   pro      0→67        875.34      1,306      1,287–1,326   gpt-5.6-sol 95%，codex-auto-review 5%
08-04 11:29   pro      0→19        271.45      1,429      1,357–1,508   gpt-5.6-sol 93%，codex-auto-review 7%
08-12 22:12   pro      0→36        517.69      1,438      1,399–1,479   gpt-5.6-sol 92%，codex-auto-review 8%
08-16 04:32   pro      0→17        248.18      1,460      1,379–1,551   gpt-5.6-sol 88%，codex-auto-review 12%
```

额度减半之后，同样以 gpt-5.6-sol 为主的一周，「整周额度≈$」这一列应该掉到一半左右。

### 2. 用量折合 API 价值

```bash
python3 codex_cost.py                          # 全部历史，按月
python3 codex_cost.py --by day --since 2026-09-01
python3 codex_cost.py --provider all           # 连同第三方供应商
python3 codex_cost.py --json > rows.json       # 逐请求明细
```

按月/周/日、按客户端、按模型、按速度档（快速模式）分别列出请求数、token、API 价值，以及其中多少是长上下文和快速模式的加价。

**桌面端、VS Code 插件、命令行（含 `codex exec`）、Chrome 侧栏、iPhone 远程控制**的会话都写在同一个 `~/.codex`（`CODEX_HOME`）里，
工具一次全扫，按会话记录里写的发起客户端分开统计。

## 安装与用法

需要 Python 3.9 以上，只用标准库，不联网，数据不出本机。

```bash
git clone https://github.com/danielli20000912-ctrl/fuck-openai.git
cd fuck-openai
python3 codex_cost.py quota
```

- 第一次运行会把会话记录全读一遍（作者 22GB 约 70 秒），结果缓存在 `.cache/`，之后只读新增的文件，几秒出结果。
- 默认扫 `$CODEX_HOME/sessions` 和 `$CODEX_HOME/archived_sessions`。旧会话搬到别处了的，复制 `config.example.json` 为
  `config.json`，把目录写进 `extra_roots`。
- 价格表在 `prices.json`，每个模型一行，都附官方页面链接。OpenAI 调价时改这个文件。

## 为什么别的工具算不准

作者用 [ccusage](https://github.com/ccusage/ccusage) 20.0.20 和本工具扫同一批文件（`checks/compare_ccusage.py`）：
ccusage 离线价格 **$646**，联网拉最新价格 **$1,084**，本工具 **$1,334**。两边的 token 数逐模型几乎一致，差距全在计价：

| 问题 | ccusage | 本工具 |
|---|---|---|
| 新模型价格 | 离线价格表没有 gpt-6-astra / 6-sol / 6.1-sol，按 $0 算 | `prices.json` 逐条核对官方页 |
| 长上下文（单次输入 > 272K 整单输入×2、输出×1.5） | gpt-6 系列不加；gpt-5.6-sol 用 LiteLLM 的 20 万阈值 | 按 OpenAI 模型页的 272K |
| 快速模式 | 旧日志没记速度档的请求也按快速模式翻倍 | 逐请求读当时的设置，没记录的按标准档 |
| `codex-auto-review`（「帮我审批」用的模型） | 认不出时按 gpt-5.5 默认价，新版映射成 gpt-5.6-luna | 按 gpt-5.6-terra |
| 按客户端拆分 | 不支持 | 桌面端 / VS Code / 命令行 / Chrome / iPhone |
| 额度折合美元 | 不支持 | `quota` 模式 |

「gpt-5.6-sol 用 20 万阈值 + 未记录速度档按快速翻倍」这个解释，按 ccusage 的规则复算 $457.74，它实际给出 $458.06，差 0.07%。

计数上要处理的坑（ccusage 20 大多已修，早期版本和其他工具仍有）：

- 子代理 / 分叉会话的日志开头会把父会话的全部历史原样复制一份，包括用量记录。本工具只在有父子关系的会话之间去重，无关会话哪怕用量数字完全相同也照算。
- 同一次请求的用量经常连写两遍，按「累计用量 + 本次用量」去重。
- 上下文压缩后累计值会清零重起；偶尔有两个进程交错续写同一个会话。所以用「本次用量」相加，不用累计值做差。

## 局限

- **只看得到本机日志。** 同一账号在别的电脑、手机上用，或者几个人共用一个账号，那部分用量本机看不到，`quota` 算出的整周价值会偏低。
- 额度百分比是整数，涨幅不到 5% 的窗口不推算；表里的「取整误差范围」就是这个误差。
- 整周价值随模型组合变化（见上面的实测表），比较不同周时，拿同一类模型为主的窗口比。
- 快速模式按「请求时选的档」计价。官方在容量不够时会降回标准档、按标准价收，但 Codex 日志不记实际档位。
- 较早版本的 Codex 不记速度档，这部分按标准档算。
- 一张现价表套用全部历史（gpt-5.6-sol 现在是促销价）。
- `codex-auto-review` 官方没公布价格，按 gpt-5.6-terra 算。

## 其他

- 测试：`python3 -m unittest discover -s tests`（人造会话记录，金额手算）。
- `checks/` 里是作者对账用的脚本：累计值自洽检查、会话库与日志文件的覆盖检查、和 ccusage 对账。
- 本项目与 OpenAI 无任何关系。实测数据来自作者自己的账号。欢迎把你的 `quota` 输出（去掉隐私）贴到 issue 里，一起攒数据。

MIT License
