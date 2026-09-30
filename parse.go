package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"time"
)

// Meta 是会话文件第一条 session_meta 里用得到的字段。
type Meta struct {
	ID           string
	TS           int64 // 毫秒
	Originator   string
	Subagent     string // "" = 主会话；"spawn" = 派生子代理；其他 = 子代理类型（如 guardian）
	ParentID     string
	ForkedFromID string
	Provider     string
}

// RateLimit 是用量记录附带的额度快照。百分比缺失时 Used 为 NaN。
type RateLimit struct {
	Plan, LimitID int32 // 字符串表下标
	PUsed         float64
	PWin, PReset  int64
	SUsed         float64
	SWin, SReset  int64
}

// Event 是一条带用量的 token_count 记录。
type Event struct {
	TS                int64
	Model, Tier, Prov int32 // 字符串表下标，0 = 未设置
	Last, Total       [6]int64
	RL                *RateLimit
}

// FileData 是一个会话文件解析后的结果（会被缓存）。
type FileData struct {
	Meta     *Meta
	OtherIDs []string
	Strs     []string // 字符串表，下标 0 恒为 ""
	Events   []Event
}

func (d *FileData) Str(i int32) string { return d.Strs[i] }

var usageFields = [6]string{"input_tokens", "cached_input_tokens", "cache_write_input_tokens",
	"output_tokens", "reasoning_output_tokens", "total_tokens"}

const (
	kNone = iota
	kSessionMeta
	kTurnContext
	kTokenCount
	kSettings
)

var (
	pTimestamp  = []byte(`{"timestamp":"`)
	pOrdinal    = []byte(`"ordinal":`)
	pType       = []byte(`"type":"`)
	pMeta       = []byte(`session_meta"`)
	pTurn       = []byte(`turn_context"`)
	pTokenCount = []byte(`event_msg","payload":{"type":"token_count"`)
	pSettings   = []byte(`event_msg","payload":{"type":"thread_settings_applied"`)
)

// classify 只看行首 250 字节认事件类型。不能在整行里搜关键字——对话正文里提到 token_count 也会被误认。
func classify(h []byte) int {
	if len(h) > 250 {
		h = h[:250]
	}
	if !bytes.HasPrefix(h, pTimestamp) {
		return kNone
	}
	i := len(pTimestamp)
	j := bytes.IndexByte(h[i:], '"')
	if j < 0 {
		return kNone
	}
	i += j + 1
	if i >= len(h) || h[i] != ',' {
		return kNone
	}
	i++
	if bytes.HasPrefix(h[i:], pOrdinal) {
		k := i + len(pOrdinal)
		n := k
		for n < len(h) && h[n] >= '0' && h[n] <= '9' {
			n++
		}
		if n == k || n >= len(h) || h[n] != ',' {
			return kNone
		}
		i = n + 1
	}
	if !bytes.HasPrefix(h[i:], pType) {
		return kNone
	}
	rest := h[i+len(pType):]
	switch {
	case bytes.HasPrefix(rest, pTokenCount):
		return kTokenCount
	case bytes.HasPrefix(rest, pTurn):
		return kTurnContext
	case bytes.HasPrefix(rest, pSettings):
		return kSettings
	case bytes.HasPrefix(rest, pMeta):
		return kSessionMeta
	}
	return kNone
}

func parseMs(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

type lineJSON struct {
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// isEmptyObject 对应 Python 里「空 dict / None 都算假」。
func isEmptyObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return nil, true
	}
	return m, false
}

func usageOf(raw json.RawMessage) [6]int64 {
	var out [6]int64
	m, empty := isEmptyObject(raw)
	if empty {
		return out
	}
	for i, k := range usageFields {
		var v *float64
		if json.Unmarshal(m[k], &v) == nil && v != nil {
			out[i] = int64(*v)
		}
	}
	return out
}

func str(raw json.RawMessage) (string, bool) {
	var s *string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == nil {
		return "", false
	}
	return *s, true
}

func num(raw json.RawMessage) (float64, bool) {
	var f *float64
	if len(raw) == 0 || json.Unmarshal(raw, &f) != nil || f == nil {
		return 0, false
	}
	return *f, true
}

type parser struct {
	d     *FileData
	index map[string]int32
}

func (p *parser) intern(s string) int32 {
	if i, ok := p.index[s]; ok {
		return i
	}
	i := int32(len(p.d.Strs))
	p.d.Strs = append(p.d.Strs, s)
	p.index[s] = i
	return i
}

func (p *parser) rateLimits(raw json.RawMessage) *RateLimit {
	m, empty := isEmptyObject(raw)
	if empty {
		return nil
	}
	rl := &RateLimit{PUsed: math.NaN(), SUsed: math.NaN()}
	if s, ok := str(m["plan_type"]); ok {
		rl.Plan = p.intern(s)
	}
	if s, ok := str(m["limit_id"]); ok {
		rl.LimitID = p.intern(s)
	}
	win := func(raw json.RawMessage) (float64, int64, int64) {
		used, w, r := math.NaN(), int64(0), int64(0)
		wm, empty := isEmptyObject(raw)
		if empty {
			return used, w, r
		}
		if v, ok := num(wm["used_percent"]); ok {
			used = v
		}
		if v, ok := num(wm["window_minutes"]); ok {
			w = int64(v)
		}
		if v, ok := num(wm["resets_at"]); ok {
			r = int64(v)
		}
		return used, w, r
	}
	rl.PUsed, rl.PWin, rl.PReset = win(m["primary"])
	rl.SUsed, rl.SWin, rl.SReset = win(m["secondary"])
	return rl
}

// truthy 对应 Python 的真值判断（null、""、0、false、空对象、空数组都算假）。
func truthy(raw json.RawMessage) bool {
	switch t := string(bytes.TrimSpace(raw)); t {
	case "", "null", `""`, "0", "false", "{}", "[]":
		return false
	}
	return true
}

// subagentKind：source 是 {"subagent": ...} 时取子代理类型和父会话，判断顺序与原 Python 版一致。
func subagentKind(raw json.RawMessage) (kind, parent string) {
	m, empty := isEmptyObject(raw)
	if empty {
		return "", ""
	}
	sa, ok := m["subagent"]
	if !ok {
		return "", ""
	}
	t := bytes.TrimSpace(sa)
	if len(t) == 0 || t[0] != '{' { // 不是对象：原样转成字符串
		if s, ok := str(sa); ok {
			return s, ""
		}
		if string(t) == "null" {
			return "None", ""
		}
		return string(t), ""
	}
	var sm map[string]json.RawMessage
	_ = json.Unmarshal(sa, &sm)
	if ts, ok := sm["thread_spawn"]; ok {
		tm, _ := isEmptyObject(ts)
		pid, _ := str(tm["parent_thread_id"])
		return "spawn", pid
	}
	if o, ok := sm["other"]; ok && truthy(o) {
		if s, ok := str(o); ok {
			return s, ""
		}
		return string(bytes.TrimSpace(o)), ""
	}
	// 取 JSON 里的第一个键（Python 的 dict 保留顺序）
	dec := json.NewDecoder(bytes.NewReader(sa))
	if _, err := dec.Token(); err == nil {
		if k, err := dec.Token(); err == nil {
			if ks, ok := k.(string); ok {
				return ks, ""
			}
		}
	}
	return "other", ""
}

// ParseFile 读一个 rollout-*.jsonl。行可能长达几百 MB，只把认得的五类行读全。
func ParseFile(path string) (*FileData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := &parser{d: &FileData{Strs: []string{""}}, index: map[string]int32{"": 0}}
	var model, tier, prov int32
	others := map[string]bool{}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) == 0 && err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		kind := classify(chunk)
		line := chunk
		if err == bufio.ErrBufferFull {
			if kind == kNone {
				for err == bufio.ErrBufferFull {
					_, err = r.ReadSlice('\n')
				}
			} else {
				buf := append([]byte(nil), chunk...)
				for err == bufio.ErrBufferFull {
					chunk, err = r.ReadSlice('\n')
					buf = append(buf, chunk...)
				}
				line = buf
			}
		}
		if kind != kNone {
			var lj lineJSON
			if json.Unmarshal(line, &lj) == nil {
				pl, _ := isEmptyObject(lj.Payload)
				switch kind {
				case kTokenCount:
					info, empty := isEmptyObject(pl["info"])
					if !empty {
						p.d.Events = append(p.d.Events, Event{
							TS: parseMs(lj.Timestamp), Model: model, Tier: tier, Prov: prov,
							Last: usageOf(info["last_token_usage"]), Total: usageOf(info["total_token_usage"]),
							RL: p.rateLimits(pl["rate_limits"]),
						})
					}
				case kTurnContext:
					if s, ok := str(pl["model"]); ok && s != "" {
						model = p.intern(s)
					}
					if s, ok := str(pl["service_tier"]); ok {
						tier = p.intern(s)
					}
				case kSettings:
					ts, _ := isEmptyObject(pl["thread_settings"])
					if s, ok := str(ts["model"]); ok && s != "" {
						model = p.intern(s)
					}
					if raw, ok := ts["service_tier"]; ok {
						s, _ := str(raw) // null → 重置为未设置
						tier = p.intern(s)
					}
					if s, ok := str(ts["model_provider_id"]); ok && s != "" {
						prov = p.intern(s)
					}
				case kSessionMeta:
					id, _ := str(pl["id"])
					if p.d.Meta == nil {
						ts, ok := str(pl["timestamp"])
						if !ok || ts == "" {
							ts = lj.Timestamp
						}
						m := &Meta{ID: id, TS: parseMs(ts)}
						m.Originator, _ = str(pl["originator"])
						m.ForkedFromID, _ = str(pl["forked_from_id"])
						m.Provider, _ = str(pl["model_provider"])
						m.Subagent, m.ParentID = subagentKind(pl["source"])
						p.d.Meta = m
						prov = p.intern(m.Provider)
					} else if id != "" && id != p.d.Meta.ID && !others[id] {
						others[id] = true
						p.d.OtherIDs = append(p.d.OtherIDs, id)
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil && err != bufio.ErrBufferFull {
			return nil, err
		}
	}
	return p.d, nil
}
