// fuck-openai：把本机 Codex（桌面端 / VS Code / 命令行 / Chrome 侧栏 / iPhone 远程）的每一次请求
// 按 OpenAI API 官方价折成美元，并用服务器返回的周额度百分比反推整周额度值多少钱。
//
// 数据来源：Codex 写在本地的会话记录 rollout-*.jsonl（所有客户端共用 CODEX_HOME，默认 ~/.codex），
// 外加搬到别处的旧会话目录（--root 或配置文件 extra_roots）。单个二进制，不联网。
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

//go:embed prices.json
var embeddedPrices []byte

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type config struct {
	Timezone   string   `json:"timezone"`
	ExtraRoots []string `json:"extra_roots"`
	CacheDir   string   `json:"cache_dir"`
	Prices     string   `json:"prices"`
}

type opts struct {
	mode, sub        string
	roots            []string
	since, until, by string
	provider, tz     string
	prices, cache    string
	noCache          bool
	watch            int
	jsonOut, quiet   bool
	configPath       string
}

func (o *opts) pricesLabel() string {
	if o.prices == "" {
		return "内置 prices.json"
	}
	return o.prices
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

const usageText = `用法：
  fuck-openai [usage] [参数]     用量折合 API 价值（默认）
  fuck-openai quota [参数]       本周额度实时价值 + 历史各周整周额度价值
  fuck-openai check identity     自查：累计用量与每次用量是否自洽
  fuck-openai check coverage     自查：Codex 会话库登记的会话有没有找不到日志文件的
  fuck-openai check ccusage      自查：与 ccusage 的逐模型 token 数对照（需要装 ccusage）

参数：
`

func parseArgs(args []string) (*opts, *config, error) {
	o := &opts{}
	var roots stringList
	fs := flag.NewFlagSet("fuck-openai", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText); fs.PrintDefaults() }
	fs.Var(&roots, "root", "会话目录，可多次指定；默认 CODEX_HOME/sessions、archived_sessions 加配置文件里的 extra_roots")
	fs.StringVar(&o.since, "since", "", "起始日期 YYYY-MM-DD（含）")
	fs.StringVar(&o.until, "until", "", "结束日期 YYYY-MM-DD（含）")
	fs.StringVar(&o.by, "by", "month", "汇总粒度：day / week / month")
	fs.StringVar(&o.provider, "provider", "openai", "只统计这个供应商（openai = 用 ChatGPT 账号登录的官方订阅）；all = 全部")
	fs.StringVar(&o.tz, "tz", "", "时区（默认读配置文件，再默认 Asia/Shanghai）")
	fs.StringVar(&o.prices, "prices", "", "价格表文件（默认用编译进程序的 prices.json）")
	fs.StringVar(&o.cache, "cache", "", "缓存文件（默认 ~/.cache/fuck-openai/cache.gob.gz，或配置文件的 cache_dir）")
	fs.BoolVar(&o.noCache, "no-cache", false, "不读也不写缓存")
	fs.IntVar(&o.watch, "watch", 0, "quota 模式下每隔几秒刷新一次")
	fs.BoolVar(&o.jsonOut, "json", false, "输出逐请求明细 JSON")
	fs.BoolVar(&o.quiet, "quiet", false, "不显示读取进度")
	fs.StringVar(&o.configPath, "config", "", "配置文件（默认 ~/.config/fuck-openai/config.json）")

	// 子命令可以写在参数前面或后面
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	o.mode = "usage"
	if rest := fs.Args(); len(rest) > 0 {
		o.mode = rest[0]
		rest = rest[1:]
		if o.mode == "check" {
			if len(rest) == 0 {
				return nil, nil, fmt.Errorf("check 后面要写 identity / coverage / ccusage")
			}
			o.sub, rest = rest[0], rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return nil, nil, err
		}
		if len(fs.Args()) > 0 {
			return nil, nil, fmt.Errorf("多余的参数：%s", strings.Join(fs.Args(), " "))
		}
	}
	switch o.mode {
	case "usage", "quota", "check":
	default:
		return nil, nil, fmt.Errorf("不认识的子命令 %q", o.mode)
	}
	if o.by != "day" && o.by != "week" && o.by != "month" {
		return nil, nil, fmt.Errorf("--by 只能是 day / week / month")
	}

	cfg := &config{}
	cp := o.configPath
	if cp == "" {
		cp = filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "fuck-openai", "config.json")
	}
	if b, err := os.ReadFile(cp); err == nil {
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, nil, fmt.Errorf("配置文件 %s 格式不对：%w", cp, err)
		}
	} else if o.configPath != "" {
		return nil, nil, err
	}
	if o.tz == "" {
		o.tz = cfg.Timezone
	}
	if o.tz == "" {
		o.tz = "Asia/Shanghai"
	}
	if o.prices == "" {
		o.prices = cfg.Prices
	}
	if o.cache == "" {
		dir := cfg.CacheDir
		if dir == "" {
			dir = filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "fuck-openai")
		}
		o.cache = filepath.Join(dir, "cache.gob.gz")
	}
	if o.noCache {
		o.cache = ""
	}
	o.roots = roots
	if len(o.roots) == 0 {
		h := codexHome()
		o.roots = append([]string{filepath.Join(h, "sessions"), filepath.Join(h, "archived_sessions")}, cfg.ExtraRoots...)
	}
	return o, cfg, nil
}

func main() {
	o, _, err := parseArgs(os.Args[1:])
	if err == flag.ErrHelp {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(2)
	}
	if o.mode == "check" {
		if err := runCheck(o); err != nil {
			fmt.Fprintln(os.Stderr, "错误：", err)
			os.Exit(1)
		}
		return
	}
	if o.watch > 0 {
		for {
			fmt.Print("\033[2J\033[H")
			if err := run(o); err != nil {
				fmt.Fprintln(os.Stderr, "错误：", err)
			}
			time.Sleep(time.Duration(o.watch) * time.Second)
		}
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func loadLocation(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("UTC+8", 8*3600)
}

func loadPricesFor(o *opts) (*Pricing, error) {
	b := embeddedPrices
	if o.prices != "" {
		var err error
		if b, err = os.ReadFile(o.prices); err != nil {
			return nil, err
		}
	}
	return loadPricing(b)
}

// compute 读取、去重、计价，返回全部供应商的行和按参数筛过的行。
func compute(o *opts, loc *time.Location) (*Loaded, DedupStats, *Pricing, []Row, []Row, error) {
	p, err := loadPricesFor(o)
	if err != nil {
		return nil, DedupStats{}, nil, nil, nil, err
	}
	L := loadAll(o.roots, o.cache, o.quiet || o.watch > 0)
	uniq, st := dedupEvents(L, lineageGroups(L))
	rows := buildRows(uniq, L, p, loc)
	var kept []Row
	for _, r := range rows {
		if (o.since == "" || r.Day >= o.since) && (o.until == "" || r.Day <= o.until) {
			kept = append(kept, r)
		}
	}
	all := kept
	if o.provider != "all" {
		var f []Row
		for _, r := range kept {
			if r.Provider == o.provider {
				f = append(f, r)
			}
		}
		kept = f
	}
	return L, st, p, kept, all, nil
}

type jsonRow struct {
	TS         int64   `json:"ts"`
	Day        string  `json:"day"`
	Client     string  `json:"client"`
	Originator string  `json:"originator"`
	Subagent   string  `json:"subagent"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Tier       string  `json:"tier"`
	Input      int64   `json:"input"`
	Cached     int64   `json:"cached"`
	CacheWrite int64   `json:"cache_write"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning"`
	USD        float64 `json:"usd"`
	USDShort   float64 `json:"usd_short"`
	IsLong     bool    `json:"is_long"`
	Known      bool    `json:"known"`
	Credits    float64 `json:"credits"`
	File       string  `json:"file"`
}

func run(o *opts) error {
	loc := loadLocation(o.tz)
	t0 := time.Now()
	L, st, p, rows, all, err := compute(o, loc)
	if err != nil {
		return err
	}
	if o.jsonOut {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS < rows[j].TS })
		out := make([]jsonRow, len(rows))
		for i, r := range rows {
			out[i] = jsonRow{r.TS, r.Day, r.Client, r.Originator, r.Subagent, r.Provider, r.Model, r.Tier,
				r.Input, r.Cached, r.CacheWrite, r.Output, r.Reasoning, r.USD, r.USDShort, r.IsLong, r.Known,
				r.Credits, filepath.Base(r.File)}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if o.mode == "quota" {
		fmt.Printf("# Codex 周额度价值（供应商=%s，时区 %s，%s）\n", o.provider, o.tz, time.Now().In(loc).Format("2006-01-02 15:04:05"))
		miss := ""
		if len(L.Missing) > 0 {
			miss = "；⚠ 未挂载/不存在：" + strings.Join(L.Missing, ", ")
		}
		label := "内置 prices.json"
		if o.prices != "" {
			label = filepath.Base(o.prices)
		}
		fmt.Printf("价格表 %s（核对日期 %s）；文件 %d 个，去重后请求 %s 次，耗时 %.1fs%s\n",
			label, p.Checked, len(L.Files), commaI(st.Unique), time.Since(t0).Seconds(), miss)
		printQuota(rows, loc, 5)
		return nil
	}
	printUsage(o, L, st, p, rows, all, time.Since(t0), codexHome())
	return nil
}
