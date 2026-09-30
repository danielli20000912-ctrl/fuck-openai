package main

// 用人造的会话记录验证去重和计价。金额都是手算的，价格取 prices.json。

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var resetAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

func ts(sec int) string {
	return time.Date(2026, 9, 1, 0, 0, sec, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
}

func line(t int, typ string, payload any) string {
	b, _ := json.Marshal(map[string]any{"timestamp": ts(t), "type": typ, "payload": payload})
	// encoding/json 按键名排序；真实日志是 timestamp、type、payload 的顺序，这里手工拼
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return `{"timestamp":` + string(m["timestamp"]) + `,"type":` + string(m["type"]) + `,"payload":` + string(m["payload"]) + `}`
}

func meta(t int, id, originator string, extra map[string]any) string {
	p := map[string]any{"id": id, "timestamp": ts(t), "originator": originator, "model_provider": "openai", "source": "vscode"}
	for k, v := range extra {
		p[k] = v
	}
	return line(t, "session_meta", p)
}

func turn(t int, model string) string { return line(t, "turn_context", map[string]any{"model": model}) }

func settings(t int, tier string) string {
	// payload 里 type 必须排第一（工具只看行首认类型），encoding/json 会按键名排序，所以手工拼
	return `{"timestamp":"` + ts(t) + `","type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"` + tier + `"}}}`
}

func usage(in, cached, out int) map[string]any {
	return map[string]any{"input_tokens": in, "cached_input_tokens": cached, "output_tokens": out,
		"reasoning_output_tokens": 0, "total_tokens": in + out}
}

func tc(t int, last, total [3]int, used float64) string {
	info, _ := json.Marshal(map[string]any{"last_token_usage": usage(last[0], last[1], last[2]),
		"total_token_usage": usage(total[0], total[1], total[2])})
	p := `{"type":"token_count","info":` + string(info)
	if !math.IsNaN(used) {
		rl, _ := json.Marshal(map[string]any{"limit_id": "codex", "plan_type": "pro", "secondary": nil,
			"primary": map[string]any{"used_percent": used, "window_minutes": 10080, "resets_at": resetAt}})
		p += `,"rate_limits":` + string(rl)
	}
	return `{"timestamp":"` + ts(t) + `","type":"event_msg","payload":` + p + `}}`
}

var nan = math.NaN()

// 父会话（桌面端，gpt-6-astra）：一次普通请求（写了两遍）、一次超过 272K 的请求、切到快速模式后一次请求
var (
	p1     = tc(10, [3]int{100_000, 60_000, 1_000}, [3]int{100_000, 60_000, 1_000}, 10)
	p2     = tc(20, [3]int{300_000, 200_000, 2_000}, [3]int{400_000, 260_000, 3_000}, 12)
	parent = []string{
		meta(0, "P", "Codex Desktop", nil),
		turn(1, "gpt-6-astra"),
		p1, p1,
		// 对话正文里出现 token_count 字样，不能被当成事件
		line(15, "response_item", map[string]any{"type": "message", "content": `"type":"event_msg","payload":{"type":"token_count"`}),
		p2,
		settings(25, "priority"),
		tc(30, [3]int{50_000, 0, 0}, [3]int{450_000, 260_000, 3_000}, 13),
	}
	// 子代理（命令行，从 P 分叉）：开头复制 P 的历史（时间戳改成分叉时刻，真实日志就是这样），然后自己用 gpt-6-sol 发一次请求
	child = []string{
		meta(100, "C", "codex-tui", map[string]any{"forked_from_id": "P",
			"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "P"}}}}),
		meta(100, "P", "Codex Desktop", nil),
		turn(100, "gpt-6-astra"),
		strings.Replace(p1, ts(10), ts(100), 1), strings.Replace(p1, ts(10), ts(100), 1), strings.Replace(p2, ts(20), ts(100), 1),
		turn(110, "gpt-6-sol"),
		tc(120, [3]int{10_000, 0, 1_000}, [3]int{410_000, 260_000, 4_000}, nan),
	}
	// 无关会话（VSCode）：用量数字恰好和 P 的第一次请求完全一样，必须照算
	unrelated = []string{meta(200, "U", "codex_vscode", nil), turn(201, "gpt-6-astra"),
		tc(210, [3]int{100_000, 60_000, 1_000}, [3]int{100_000, 60_000, 1_000}, nan)}
)

func setup(t *testing.T, files map[string][]string) ([]Row, DedupStats, *Loaded) {
	t.Helper()
	dir := t.TempDir()
	for name, lines := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	L := loadAll([]string{dir}, filepath.Join(dir, "cache.gob.gz"), true)
	uniq, st := dedupEvents(L, lineageGroups(L))
	p, err := loadPricing(embeddedPrices)
	if err != nil {
		t.Fatal(err)
	}
	return buildRows(uniq, L, p, time.UTC), st, L
}

func total(rows []Row, client string) float64 {
	s := 0.0
	for _, r := range rows {
		if r.Client == client {
			s += r.USD
		}
	}
	return s
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v，应为 %v", name, got, want)
	}
}

func fixture(t *testing.T) ([]Row, DedupStats) {
	rows, st, _ := setup(t, map[string][]string{"rollout-P.jsonl": parent, "rollout-C.jsonl": child, "rollout-U.jsonl": unrelated})
	return rows, st
}

func TestDedup(t *testing.T) {
	rows, st := fixture(t)
	// P 3 次 + C 自己 1 次 + U 1 次；P 的重复写入和 C 复制来的历史都去掉
	if len(rows) != 5 || st.Raw != 4+4+1 {
		t.Fatalf("去重后 %d 条（应为 5），原始 %d 条（应为 9）", len(rows), st.Raw)
	}
}

func TestParentCost(t *testing.T) {
	rows, _ := fixture(t)
	// 普通：(40K×10 + 60K×1 + 1K×50)/1M = 0.51
	// 长上下文：(100K×10×2 + 200K×1×2 + 2K×50×1.5)/1M = 2.55
	// 快速模式：50K×10/1M×2 = 1.00
	near(t, "桌面端", total(rows, "桌面端"), 4.06)
	long := 0
	for _, r := range rows {
		if r.IsLong {
			long++
		}
	}
	if long != 1 {
		t.Errorf("长上下文请求 %d 次，应为 1", long)
	}
}

func TestChildCountsOnlyItsOwnRequest(t *testing.T) {
	rows, _ := fixture(t)
	// gpt-6-sol：(10K×2 + 1K×10)/1M = 0.03
	near(t, "命令行", total(rows, "命令行"), 0.03)
}

func TestUnrelatedSessionNotMerged(t *testing.T) {
	rows, st := fixture(t)
	near(t, "VSCode", total(rows, "VSCode"), 0.51)
	if st.CrossGroup != 1 {
		t.Errorf("不同会话间用量相同 %d 条，应为 1", st.CrossGroup)
	}
}

func TestTieGoesToParent(t *testing.T) {
	// 复制记录的时间戳和原件完全相同时，也要归给父会话
	_, _, L := setup(t, map[string][]string{
		"rollout-A-child.jsonl":  {meta(100, "C", "codex-tui", map[string]any{"forked_from_id": "P"}), meta(100, "P", "Codex Desktop", nil), turn(1, "gpt-6-astra"), p1},
		"rollout-B-parent.jsonl": parent[:3],
	})
	uniq, _ := dedupEvents(L, lineageGroups(L))
	if len(uniq) != 1 || filepath.Base(uniq[0].File) != "rollout-B-parent.jsonl" {
		t.Fatalf("应只剩父会话的一条，实际 %v", uniq)
	}
}

func TestQuotaWindow(t *testing.T) {
	rows, _ := fixture(t)
	ws := quotaWindows(rows)
	if len(ws) != 1 {
		t.Fatalf("窗口 %d 个，应为 1", len(ws))
	}
	w := ws[0]
	// 10% → 13%，这之间的用量 = 2.55 + 1.00，整周 = 3.55 / 3 × 100
	if w.U0 != 10 || w.U1 != 13 {
		t.Errorf("已用 %v→%v，应为 10→13", w.U0, w.U1)
	}
	near(t, "整周价值", *w.WeekUSD, 3.55/3*100)
}

func TestLongLineAndMissingNewline(t *testing.T) {
	// 超过读缓冲的超长行（真实日志里压缩记录能到几百 MB），以及最后一行没有换行符
	big := line(5, "response_item", map[string]any{"type": "message", "content": strings.Repeat("x", 3<<20)})
	bigMeta := meta(0, "L", "codex_vscode", map[string]any{"base_instructions": strings.Repeat("y", 2<<20)})
	dir := t.TempDir()
	body := strings.Join([]string{bigMeta, turn(1, "gpt-6-astra"), big,
		tc(10, [3]int{100_000, 60_000, 1_000}, [3]int{100_000, 60_000, 1_000}, nan)}, "\n") // 末尾无换行
	if err := os.WriteFile(filepath.Join(dir, "rollout-L.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := ParseFile(filepath.Join(dir, "rollout-L.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta == nil || d.Meta.ID != "L" || len(d.Events) != 1 || d.Str(d.Events[0].Model) != "gpt-6-astra" {
		t.Fatalf("超长行解析不对：meta=%+v events=%d", d.Meta, len(d.Events))
	}
}
