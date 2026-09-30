package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func runCheck(o *opts) error {
	switch o.sub {
	case "identity":
		checkIdentity(o)
	case "coverage":
		return checkCoverage(o)
	case "ccusage":
		return checkCcusage(o)
	default:
		return fmt.Errorf("check 只支持 identity / coverage / ccusage")
	}
	return nil
}

// checkIdentity：累计用量与每次用量是否自洽。
//
// 在每个文件里按顺序走，累计值（输入/缓存/输出）变小的地方视为计数器重起（上下文压缩后会这样），
// 每一段里检查：Σ(累计值有变化的记录的本次用量) == 段末累计 − 段首累计 + 段首本次用量。
// 另外统计「累计值没变、本次用量却不为零」的记录。
func checkIdentity(o *opts) {
	L := loadAll(o.roots, o.cache, o.quiet)
	F := [3]int{0, 1, 3}
	var okFiles, badFiles, segTotal, segBad, staleNonzero int
	var stale [3]int64
	type bad struct {
		name     string
		got, exp [3]int64
	}
	var bads []bad
	for _, f := range L.Files {
		ev := L.Data[f].Events
		if len(ev) == 0 {
			continue
		}
		segs := [][]*Event{{}}
		var prev *[3]int64
		for i := range ev {
			e := &ev[i]
			tot := [3]int64{e.Total[F[0]], e.Total[F[1]], e.Total[F[2]]}
			if prev != nil && (tot[0] < prev[0] || tot[1] < prev[1] || tot[2] < prev[2]) {
				segs = append(segs, nil)
			}
			if prev != nil && tot == *prev {
				last := [3]int64{e.Last[F[0]], e.Last[F[1]], e.Last[F[2]]}
				if last != [3]int64{} {
					staleNonzero++
					for k := range last {
						stale[k] += last[k]
					}
				}
				continue
			}
			segs[len(segs)-1] = append(segs[len(segs)-1], e)
			t := tot
			prev = &t
		}
		fileOK := true
		for _, s := range segs {
			if len(s) == 0 {
				continue
			}
			segTotal++
			var got, exp [3]int64
			for k, fi := range F {
				for _, e := range s {
					got[k] += e.Last[fi]
				}
				exp[k] = s[len(s)-1].Total[fi] - s[0].Total[fi] + s[0].Last[fi]
			}
			if got != exp {
				segBad++
				fileOK = false
				name := filepath.Base(f)
				if r := []rune(name); len(r) > 62 {
					name = string(r[:62])
				}
				bads = append(bads, bad{name, got, exp})
			}
		}
		if fileOK {
			okFiles++
		} else {
			badFiles++
		}
	}
	fmt.Printf("文件：自洽 %d，不自洽 %d；分段 %d，不自洽段 %d\n", okFiles, badFiles, segTotal, segBad)
	fmt.Printf("累计值没变但本次用量非零的记录：%d 条，输入/缓存/输出 = %v\n", staleNonzero, stale)
	for i, b := range bads {
		if i == 10 {
			break
		}
		fmt.Printf("   %s 本次用量之和 %v，按累计值应为 %v\n", b.name, b.got, b.exp)
	}
}

// checkCoverage：Codex 会话库（state_5.sqlite）登记的会话，哪些在所有扫描目录里都找不到记录文件。
func checkCoverage(o *opts) error {
	L := loadAll(o.roots, o.cache, o.quiet)
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return fmt.Errorf("需要系统里有 sqlite3 命令")
	}
	db := filepath.Join(codexHome(), "state_5.sqlite")
	out, err := exec.Command(bin, "-readonly", "-separator", "\t", db,
		"select id, created_at, coalesce(originator,''), model_provider, tokens_used, coalesce(thread_source,'') from threads").Output()
	if err != nil {
		return fmt.Errorf("读不了 %s：%v", db, err)
	}
	have := map[string]bool{}
	for _, f := range L.Files {
		if m := L.Data[f].Meta; m != nil {
			have[m.ID] = true
		}
	}
	type key struct{ month, orig, prov, kind string }
	count := map[key]int{}
	tokens := map[key]int64{}
	dbIDs := map[string]bool{}
	total, lost := 0, 0
	for _, ln := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(ln, "\t")
		if len(f) < 6 {
			continue
		}
		total++
		dbIDs[f[0]] = true
		if have[f[0]] {
			continue
		}
		lost++
		created, _ := strconv.ParseInt(f[1], 10, 64)
		orig := f[2]
		if orig == "" {
			orig = "-"
		}
		kind := "sub"
		if f[5] == "user" {
			kind = "user"
		}
		k := key{time.Unix(created, 0).In(time.Local).Format("2006-01"), orig, f[3], kind}
		count[k]++
		tok, _ := strconv.ParseInt(f[4], 10, 64)
		tokens[k] += tok
	}
	fmt.Printf("会话库登记会话 %d，有记录文件 %d，找不到文件 %d\n", total, total-lost, lost)
	var keys []key
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return a.month+"\x00"+a.orig+"\x00"+a.prov+"\x00"+a.kind < b.month+"\x00"+b.orig+"\x00"+b.prov+"\x00"+b.kind
	})
	for _, k := range keys {
		fmt.Printf("   %s %s %s %s：%d 个，会话库记的 tokens_used 合计 %.1fM\n", k.month, k.orig, k.prov, k.kind, count[k], float64(tokens[k])/1e6)
	}
	extra := 0
	for _, f := range L.Files {
		if m := L.Data[f].Meta; m != nil && !dbIDs[m.ID] {
			extra++
		}
	}
	fmt.Printf("有记录文件但会话库没登记：%d 个\n", extra)
	return nil
}

// checkCcusage：与 ccusage 在同一批文件上逐月逐模型对照 token 数。
// 两边都只扫 CODEX_HOME/sessions + archived_sessions、全部供应商、同一时区。
func checkCcusage(o *opts) error {
	bin, err := exec.LookPath("ccusage")
	if err != nil {
		return fmt.Errorf("需要先安装 ccusage")
	}
	raw, err := exec.Command(bin, "codex", "monthly", "--json", "--offline", "-z", o.tz).Output()
	if err != nil {
		return fmt.Errorf("ccusage 运行失败：%v", err)
	}
	var cu struct {
		Monthly []struct {
			Month  string `json:"month"`
			Models map[string]struct {
				Input     float64 `json:"inputTokens"`
				CacheRead float64 `json:"cacheReadTokens"`
				Output    float64 `json:"outputTokens"`
			} `json:"models"`
		} `json:"monthly"`
	}
	if err := json.Unmarshal(raw, &cu); err != nil {
		return fmt.Errorf("看不懂 ccusage 的输出：%v", err)
	}
	h := codexHome()
	o2 := *o
	o2.roots = []string{filepath.Join(h, "sessions"), filepath.Join(h, "archived_sessions")}
	o2.provider = "all"
	loc := loadLocation(o.tz)
	_, _, _, rows, _, err := compute(&o2, loc)
	if err != nil {
		return err
	}
	type k2 struct{ month, model string }
	type tok struct{ unc, cached, out float64 }
	mine := map[k2]*tok{}
	theirs := map[k2]*tok{}
	for _, r := range rows {
		k := k2{r.Day[:7], r.Model}
		if mine[k] == nil {
			mine[k] = &tok{}
		}
		mine[k].unc += float64(r.Input - r.Cached)
		mine[k].cached += float64(r.Cached)
		mine[k].out += float64(r.Output)
	}
	for _, m := range cu.Monthly {
		for model, v := range m.Models {
			theirs[k2{m.Month, model}] = &tok{v.Input, v.CacheRead, v.Output}
		}
	}
	keys := map[k2]bool{}
	for k := range mine {
		keys[k] = true
	}
	for k := range theirs {
		keys[k] = true
	}
	var ks []k2
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].month != ks[j].month {
			return ks[i].month < ks[j].month
		}
		return ks[i].model < ks[j].model
	})
	f := func(t *tok) string {
		if t == nil {
			t = &tok{}
		}
		return fmt.Sprintf("%.1f/%.1f/%.1f", t.unc/1e6, t.cached/1e6, t.out/1e6)
	}
	fmt.Println(padR("月份", 8) + " " + padR("模型", 20) + " " + padL("ccusage 未缓存/缓存/输出 (M)", 30) + " " + padL("本工具 未缓存/缓存/输出 (M)", 30))
	for _, k := range ks {
		fmt.Println(padR(k.month, 8) + " " + padR(k.model, 20) + " " + padL(f(theirs[k]), 30) + " " + padL(f(mine[k]), 30))
	}
	return nil
}
