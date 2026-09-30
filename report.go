package main

import (
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var clientLabels = map[string]string{
	"Codex Desktop":                    "桌面端",
	"codex_work_desktop":               "桌面端(Work)",
	"codex_vscode":                     "VSCode",
	"codex-tui":                        "命令行",
	"codex_cli_rs":                     "命令行",
	"codex_exec":                       "命令行",
	"codex-chrome-extension-sidepanel": "Chrome 侧栏",
	"codex_chatgpt_ios_remote":         "iPhone 远程",
}

func clientLabel(originator string) string {
	if originator == "" {
		return "未知客户端"
	}
	if l, ok := clientLabels[originator]; ok {
		return l
	}
	return originator
}

// rlView 是额度快照的展开形式。
type rlView struct {
	Plan, LimitID string
	PUsed         float64
	PWin, PReset  int64
	SUsed         float64
	SWin, SReset  int64
}

// Row 是一次请求的计价结果。
type Row struct {
	TS                                           int64
	Day, Client, Originator, Subagent            string
	Provider, Model, Tier                        string
	Input, Cached, CacheWrite, Output, Reasoning int64
	USD, USDShort                                float64
	IsLong, Known                                bool
	Credits                                      float64
	File                                         string
	RL                                           *rlView
}

func buildRows(uniq []Uniq, L *Loaded, p *Pricing, loc *time.Location) []Row {
	rows := make([]Row, 0, len(uniq))
	for _, u := range uniq {
		d := L.Data[u.File]
		e := u.Ev
		m := d.Meta
		if m == nil {
			m = &Meta{}
		}
		model, tier, prov := d.Str(e.Model), d.Str(e.Tier), d.Str(e.Prov)
		usd, long, tm, known := p.cost(model, tier, e.Last)
		short := p.short(model, e.Last)
		credits := short * p.creditsPerUSD
		if tier == "priority" || tier == "fast" {
			credits *= tm
		}
		r := Row{
			TS:         e.TS,
			Day:        time.UnixMilli(e.TS).In(loc).Format("2006-01-02"),
			Client:     clientLabel(m.Originator),
			Originator: m.Originator,
			Subagent:   m.Subagent,
			Provider:   firstNonEmpty(prov, m.Provider),
			Model:      firstNonEmpty(model, "(未知)"),
			Tier:       firstNonEmpty(tier, "(旧版未记录,按标准)"),
			Input:      e.Last[0], Cached: min(e.Last[1], e.Last[0]), CacheWrite: e.Last[2],
			Output: e.Last[3], Reasoning: e.Last[4],
			USD: usd, USDShort: short, IsLong: long, Known: known, Credits: credits,
			File: u.File,
		}
		if e.RL != nil {
			r.RL = &rlView{d.Str(e.RL.Plan), d.Str(e.RL.LimitID), e.RL.PUsed, e.RL.PWin, e.RL.PReset,
				e.RL.SUsed, e.RL.SWin, e.RL.SReset}
		}
		rows = append(rows, r)
	}
	return rows
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- 汇总

type aggVal struct {
	Req, Input, Cached, Output, Reasoning, USD, USDShort, LongReq, UnknownTokens float64
}

// ordered 保留键第一次出现的顺序（排序并列时与原版一致）。
type ordered struct {
	keys []string
	vals map[string]*aggVal
}

func agg(rows []Row, key func(*Row) string) *ordered {
	o := &ordered{vals: map[string]*aggVal{}}
	for i := range rows {
		r := &rows[i]
		k := key(r)
		a, ok := o.vals[k]
		if !ok {
			a = &aggVal{}
			o.vals[k] = a
			o.keys = append(o.keys, k)
		}
		a.Req++
		a.Input += float64(r.Input)
		a.Cached += float64(r.Cached)
		a.Output += float64(r.Output)
		a.Reasoning += float64(r.Reasoning)
		a.USD += r.USD
		a.USDShort += r.USDShort
		if r.IsLong {
			a.LongReq++
		}
		if !r.Known {
			a.UnknownTokens += float64(r.Input + r.Output)
		}
	}
	return o
}

// ---------------------------------------------------------------- 格式化（与原 Python 输出逐字一致）

func runeLen(s string) int { return len([]rune(s)) }

func padR(s string, w int) string {
	if n := runeLen(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func padL(s string, w int) string {
	if n := runeLen(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

// displayBytes 模拟 len(s.encode("gbk"))：ASCII 占 1，其余占 2。
func displayBytes(s string) int {
	n := 0
	for _, r := range s {
		if r < 0x80 {
			n++
		} else {
			n += 2
		}
	}
	return n
}

func groupDigits(s string) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

// commaF 对应 Python 的 f"{x:,.{prec}f}"。
func commaF(x float64, prec int) string {
	if math.IsNaN(x) {
		return "nan"
	}
	if math.IsInf(x, 0) {
		if x > 0 {
			return "inf"
		}
		return "-inf"
	}
	return groupDigits(strconv.FormatFloat(x, 'f', prec, 64))
}

func commaI(n int) string { return groupDigits(strconv.Itoa(n)) }

func fmtF(x float64, prec int) string {
	if math.IsNaN(x) {
		return "nan"
	}
	return strconv.FormatFloat(x, 'f', prec, 64)
}

func fmtTok(n float64) string {
	switch {
	case n >= 1e9:
		return fmtF(n/1e9, 2) + "B"
	case n >= 1e6:
		return fmtF(n/1e6, 1) + "M"
	case n >= 1e3:
		return fmtF(n/1e3, 1) + "K"
	}
	return fmtF(n, 0)
}

func printTable(title string, data *ordered, keyName string, byUSD bool) {
	fmt.Printf("\n## %s\n", title)
	hdr := padR(keyName, 22) + " " + padL("请求数", 7) + " " + padL("输入", 9) + " " + padL("其中缓存", 9) + " " +
		padL("输出", 8) + " " + padL("API价值$", 11) + " " + padL("其中长上下文+快速加价$", 14)
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", displayBytes(hdr)))
	keys := append([]string(nil), data.keys...)
	if byUSD {
		sort.SliceStable(keys, func(i, j int) bool { return -data.vals[keys[i]].USD < -data.vals[keys[j]].USD })
	} else {
		sort.Strings(keys)
	}
	var tot aggVal
	line := func(k string, a *aggVal) {
		fmt.Println(padR(k, 22) + " " + padL(strconv.Itoa(int(a.Req)), 7) + " " + padL(fmtTok(a.Input), 9) + " " +
			padL(fmtTok(a.Cached), 9) + " " + padL(fmtTok(a.Output), 8) + " " + padL(commaF(a.USD, 2), 11) + " " +
			padL(commaF(a.USD-a.USDShort, 2), 14))
	}
	for _, k := range keys {
		a := data.vals[k]
		line(k, a)
		tot.Req += a.Req
		tot.Input += a.Input
		tot.Cached += a.Cached
		tot.Output += a.Output
		tot.USD += a.USD
		tot.USDShort += a.USDShort
	}
	line("合计", &tot)
}

// ---------------------------------------------------------------- 会话库覆盖检查

type coverage struct {
	Threads, Lost, LostSub int
	LostUserTokens         int64
	First, Last            string
}

// dbCoverage：Codex 自己的会话库（state_5.sqlite）登记了、但所有目录都找不到记录文件的会话。
// Go 标准库没有 SQLite，调用系统的 sqlite3 命令；找不到命令或读失败就跳过这项检查。
func dbCoverage(L *Loaded, codexHome string) *coverage {
	db := filepath.Join(codexHome, "state_5.sqlite")
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin, "-readonly", "-separator", "\t", db,
		"select id, created_at, coalesce(thread_source,''), tokens_used from threads").Output()
	if err != nil {
		return nil
	}
	have := map[string]bool{}
	for _, f := range L.Files {
		if m := L.Data[f].Meta; m != nil {
			have[m.ID] = true
		}
	}
	c := &coverage{}
	var days []string
	for _, ln := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if ln == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) < 4 {
			continue
		}
		c.Threads++
		if have[f[0]] {
			continue
		}
		c.Lost++
		created, _ := strconv.ParseInt(f[1], 10, 64)
		days = append(days, time.Unix(created, 0).In(time.Local).Format("2006-01-02"))
		if f[2] == "user" {
			tok, _ := strconv.ParseInt(f[3], 10, 64)
			c.LostUserTokens += tok
		} else {
			c.LostSub++
		}
	}
	sort.Strings(days)
	if len(days) > 0 {
		c.First, c.Last = days[0], days[len(days)-1]
	}
	return c
}

// ---------------------------------------------------------------- 用量报告

func periodKey(by string) func(*Row) string {
	return func(r *Row) string {
		switch by {
		case "day":
			return r.Day
		case "month":
			return r.Day[:7]
		}
		d, _ := time.Parse("2006-01-02", r.Day)
		wd := (int(d.Weekday()) + 6) % 7 // 周一 = 0
		return d.AddDate(0, 0, -wd).Format("2006-01-02") + " 周"
	}
}

func printUsage(o *opts, L *Loaded, st DedupStats, p *Pricing, rows, allRows []Row, elapsed time.Duration, codexHome string) {
	span := "无数据"
	if len(rows) > 0 {
		lo, hi := rows[0].Day, rows[0].Day
		for _, r := range rows {
			lo, hi = min(lo, r.Day), max(hi, r.Day)
		}
		span = lo + " → " + hi
	}
	fmt.Printf("# Codex 用量按 API 价折算（%s，供应商=%s，时区 %s）\n", span, o.provider, o.tz)
	fmt.Printf("价格表：%s（核对日期 %s；所有历史用量按这一张现价表计）\n", o.pricesLabel(), p.Checked)
	fmt.Println("扫描目录：")
	missing := map[string]bool{}
	for _, m := range L.Missing {
		missing[m] = true
	}
	for _, r := range o.roots {
		mark := "✓"
		if missing[r] {
			mark = "✗ 不存在/未挂载"
		}
		fmt.Printf("  %s %s\n", mark, r)
	}
	fmt.Printf("文件 %d 个（本次新解析 %d），用量事件 %s 条，去重后 %s 条（去掉子代理复制的父会话历史和重复写入 %s 条），耗时 %.1fs\n",
		len(L.Files), L.Fresh, commaI(st.Raw), commaI(st.Unique), commaI(st.Dup), elapsed.Seconds())
	fmt.Printf("数据质量：父会话文件已丢失、只能用子会话复制的历史补回的用量 %s 条（%s token，日期记在分叉当天）；"+
		"不同会话间用量完全相同的 %s 条（多说明有没识别出的分叉）\n",
		commaI(st.ReplayOnly), fmtTok(float64(st.ReplayOnlyTokens)), commaI(st.CrossGroup))
	if c := dbCoverage(L, codexHome); c != nil && c.Lost > 0 {
		fmt.Printf("          Codex 会话库登记 %d 个会话，其中 %d 个在所有目录里都找不到记录文件"+
			"（%s → %s；子代理 %d 个，主会话 %d 个、会话库记的累计 %s token）——这部分算不到\n",
			c.Threads, c.Lost, c.First, c.Last, c.LostSub, c.Lost-c.LostSub, fmtTok(float64(c.LostUserTokens)))
	}

	byName := map[string]string{"day": "日", "week": "周", "month": "月"}[o.by]
	printTable("按"+byName, agg(rows, periodKey(o.by)), "时间", false)
	printTable("按客户端", agg(rows, func(r *Row) string { return r.Client }), "客户端", true)
	printTable("按客户端（主会话 / 子代理分开）", agg(rows, func(r *Row) string {
		if r.Subagent != "" {
			return r.Client + "·子代理"
		}
		return r.Client + "·主会话"
	}), "客户端", true)
	printTable("按模型", agg(rows, func(r *Row) string { return r.Model }), "模型", true)
	printTable("按速度档（priority = 快速模式，API 价×2）", agg(rows, func(r *Row) string { return r.Tier }), "速度档", true)
	if o.provider != "all" {
		printTable("全部供应商（对照：openai 以外的是第三方供应商）", agg(allRows, func(r *Row) string { return r.Provider }), "供应商", true)
	}

	unk := &ordered{vals: map[string]*aggVal{}}
	for _, r := range rows {
		if r.Known {
			continue
		}
		a, ok := unk.vals[r.Model]
		if !ok {
			a = &aggVal{}
			unk.vals[r.Model] = a
			unk.keys = append(unk.keys, r.Model)
		}
		a.Input += float64(r.Input + r.Output)
	}
	if len(unk.keys) > 0 {
		sort.SliceStable(unk.keys, func(i, j int) bool { return -unk.vals[unk.keys[i]].Input < -unk.vals[unk.keys[j]].Input })
		var parts []string
		for _, k := range unk.keys {
			parts = append(parts, k+" "+fmtTok(unk.vals[k].Input)+" token")
		}
		fmt.Println("\n⚠ 价格表里没有的模型（按 $0 计，需要补价）：" + strings.Join(parts, "，"))
	}
}
