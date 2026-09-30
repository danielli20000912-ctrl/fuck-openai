package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type snap struct {
	ts   int64
	used float64
}

// Window 是一个周额度窗口的反推结果。
type Window struct {
	Reset                  int64
	Plan                   string
	Snaps                  int
	T0, T1                 int64
	U0, U1, DU             float64
	USD, Credits           float64
	WeekUSD, WeekUSDLo     *float64
	WeekUSDHi, WeekCredits *float64
	Models                 []modelShare
}

type modelShare struct {
	Model string
	USD   float64
}

// weekly 从额度快照里取「codex 主桶的周窗口」：(已用%, 重置时间, 套餐)。
func weekly(rl *rlView) (float64, int64, string, bool) {
	if rl == nil || rl.LimitID != "codex" {
		return 0, 0, "", false
	}
	for _, w := range [][3]float64{{rl.PUsed, float64(rl.PWin), float64(rl.PReset)}, {rl.SUsed, float64(rl.SWin), float64(rl.SReset)}} {
		if w[1] == 10080 && !math.IsNaN(w[0]) && w[2] != 0 {
			return w[0], int64(w[2]), rl.Plan, true
		}
	}
	return 0, 0, "", false
}

func ptr(x float64) *float64 { return &x }

// quotaWindows 把请求按周额度窗口分组，反推每个窗口整周额度值多少 API 美元。
//
// 每条用量记录都附带服务器当时的周额度已用百分比。一个会话只属于一个账号，所以每条请求归到
// 它所在会话里最近一张 codex 主桶快照的窗口（不同账号的重置时间不同，靠这个区分）。
// 窗口内：第一张快照之后、最后一张快照为止的用量 ÷ 百分比涨幅 × 100 = 整周额度的价值。
func quotaWindows(rows []Row) []Window {
	var files []string
	byFile := map[string][]*Row{}
	for i := range rows {
		r := &rows[i]
		if _, ok := byFile[r.File]; !ok {
			files = append(files, r.File)
		}
		byFile[r.File] = append(byFile[r.File], r)
	}
	type win struct {
		reset int64
		plan  string
		snaps []snap
		reqs  []*Row
	}
	wins := map[int64]*win{}
	for _, f := range files {
		rs := byFile[f]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].TS < rs[j].TS })
		var cur int64
		have := false
		for _, r := range rs {
			if used, reset, plan, ok := weekly(r.RL); ok {
				cur = int64(math.RoundToEven(float64(reset) / 3600)) // 重置时间有几秒抖动，按小时归并
				have = true
				w, ok := wins[cur]
				if !ok {
					w = &win{reset: reset, plan: plan}
					wins[cur] = w
				}
				w.snaps = append(w.snaps, snap{r.TS, used})
			}
			if have {
				wins[cur].reqs = append(wins[cur].reqs, r)
			}
		}
	}
	var keys []int64
	for k := range wins {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var out []Window
	for _, k := range keys {
		W := wins[k]
		s := W.snaps
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].ts != s[j].ts {
				return s[i].ts < s[j].ts
			}
			return s[i].used < s[j].used
		})
		t0, u0, t1, u1 := s[0].ts, s[0].used, s[len(s)-1].ts, s[len(s)-1].used
		w := Window{Reset: W.reset, Plan: W.plan, Snaps: len(s), T0: t0, T1: t1, U0: u0, U1: u1, DU: u1 - u0}
		models := map[string]float64{}
		var order []string
		for _, r := range W.reqs {
			if t0 < r.TS && r.TS <= t1 {
				w.USD += r.USD
				w.Credits += r.Credits
				if _, ok := models[r.Model]; !ok {
					order = append(order, r.Model)
				}
				models[r.Model] += r.USD
			}
		}
		for _, m := range order {
			w.Models = append(w.Models, modelShare{m, models[m]})
		}
		sort.SliceStable(w.Models, func(i, j int) bool { return -w.Models[i].USD < -w.Models[j].USD })
		if w.DU > 0 {
			w.WeekUSD = ptr(w.USD / w.DU * 100)
			w.WeekUSDLo = ptr(w.USD / (w.DU + 1) * 100)
			w.WeekCredits = ptr(w.Credits / w.DU * 100)
		}
		if w.DU > 1 {
			w.WeekUSDHi = ptr(w.USD / (w.DU - 1) * 100)
		}
		out = append(out, w)
	}
	return out
}

func printQuota(rows []Row, loc *time.Location, minRise float64) {
	ws := quotaWindows(rows)
	now := time.Now().Unix()
	tm := func(t time.Time) string { return t.In(loc).Format("01-02 15:04") }
	mix := func(w Window) string {
		tot := 0.0
		for _, m := range w.Models {
			tot += m.USD
		}
		if tot == 0 {
			tot = 1
		}
		var parts []string
		for i, m := range w.Models {
			if i == 2 {
				break
			}
			parts = append(parts, m.Model+" "+fmtF(m.USD/tot*100, 0)+"%")
		}
		return strings.Join(parts, "，")
	}
	// hiOr 对应 Python 的「值为 None 或 0 时换成 nan」
	hiOr := func(p *float64) float64 {
		if p == nil || *p == 0 {
			return math.NaN()
		}
		return *p
	}
	plan := func(s string) string {
		if s == "" {
			return "?"
		}
		return s
	}

	fmt.Println("\n## 本周（还没重置的窗口）")
	n := 0
	for _, w := range ws {
		if w.Reset <= now {
			continue
		}
		n++
		fmt.Printf("套餐 %s，%s 重置；最近读数 %s 已用 %s%%\n", plan(w.Plan), tm(time.Unix(w.Reset, 0)),
			tm(time.UnixMilli(w.T1)), fmtF(w.U1, 0))
		if w.DU >= minRise {
			left := *w.WeekUSD * (100 - w.U1) / 100
			fmt.Printf("  本窗口在本机用掉 API 价值 $%s，额度涨了 %s%% → 整周额度约值 $%s（取整误差范围 $%s–$%s），剩下的 %s%% 约值 $%s\n",
				commaF(w.USD, 2), fmtF(w.DU, 0), commaF(*w.WeekUSD, 0), commaF(*w.WeekUSDLo, 0), commaF(hiOr(w.WeekUSDHi), 0),
				fmtF(100-w.U1, 0), commaF(left, 0))
		} else {
			fmt.Printf("  本窗口额度只涨了 %s%%，不到 %s%%，整数百分比的取整误差太大，先不推算\n", fmtF(w.DU, 0), strconv.FormatFloat(minRise, 'f', -1, 64))
		}
	}
	if n == 0 {
		fmt.Println("本机日志里没有本周的额度读数（这周还没在本机用过 Codex）。")
	}

	fmt.Printf("\n## 历史各周：整周额度折合 API 价值（涨幅 ≥ %s%% 的窗口才推算）\n", strconv.FormatFloat(minRise, 'f', -1, 64))
	fmt.Println(padR("重置时间", 12) + " " + padR("套餐", 5) + " " + padL("已用% 起→止", 11) + " " + padL("这段API价值$", 12) + " " +
		padL("整周额度≈$", 11) + " " + padL("取整误差范围$", 16) + " " + padL("整周≈费率卡点", 13) + "  主力模型")
	for _, w := range ws {
		if w.DU < minRise {
			continue
		}
		rng := "-"
		if w.WeekUSDHi != nil && *w.WeekUSDHi != 0 {
			rng = commaF(*w.WeekUSDLo, 0) + "–" + commaF(*w.WeekUSDHi, 0)
		}
		fmt.Println(padR(tm(time.Unix(w.Reset, 0)), 12) + " " + padR(plan(w.Plan), 5) + " " + padL(fmtF(w.U0, 0), 4) + "→" +
			padR(fmtF(w.U1, 0), 5) + " " + padL(commaF(w.USD, 2), 12) + " " + padL(commaF(*w.WeekUSD, 0), 11) + " " +
			padL(rng, 16) + " " + padL(commaF(*w.WeekCredits, 0), 13) + "  " + mix(w))
	}
	fmt.Println("\n说明：同一账号在别的电脑/手机上用、或多人共用一个账号时，那部分用量本机看不到，推算值会偏低。" +
		"整周价值随模型组合变化（同样 1% 额度，不同模型折合的 API 价差很多），比较不同周时看同类模型为主的窗口。")
}
