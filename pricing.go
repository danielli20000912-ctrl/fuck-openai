package main

import (
	"encoding/json"
	"fmt"
)

type modelPrice struct {
	Input          float64  `json:"input"`
	CachedInput    float64  `json:"cached_input"`
	Output         float64  `json:"output"`
	FastMult       *float64 `json:"fast_mult"`
	LongContext    *bool    `json:"long_context"`
	CacheWriteMult *float64 `json:"cache_write_mult"`
}

type Pricing struct {
	Checked     string                `json:"checked"`
	Models      map[string]modelPrice `json:"models"`
	Aliases     map[string]string     `json:"aliases"`
	LongContext struct {
		Threshold  int64   `json:"threshold_input_tokens"`
		InputMult  float64 `json:"input_mult"`
		CachedMult float64 `json:"cached_mult"`
		OutputMult float64 `json:"output_mult"`
	} `json:"long_context"`
	Tiers map[string]struct {
		Mult float64 `json:"mult"`
	} `json:"service_tiers"`
	RateCard struct {
		CreditsPerUSD *float64 `json:"credits_per_usd"`
	} `json:"rate_card"`
	creditsPerUSD float64
}

func loadPricing(b []byte) (*Pricing, error) {
	var p Pricing
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("价格表格式不对：%w", err)
	}
	p.creditsPerUSD = 25
	if p.RateCard.CreditsPerUSD != nil {
		p.creditsPerUSD = *p.RateCard.CreditsPerUSD
	}
	return &p, nil
}

func (p *Pricing) resolve(model string) (*modelPrice, bool) {
	if model == "" {
		return nil, false
	}
	name := model
	if a, ok := p.Aliases[model]; ok {
		name = a
	}
	m, ok := p.Models[name]
	if !ok {
		return nil, false
	}
	return &m, true
}

func (m *modelPrice) cwMult() float64 {
	if m.CacheWriteMult != nil {
		return *m.CacheWriteMult
	}
	return 1.25
}

// tierMult：快速模式（日志里叫 priority）按模型的 fast_mult，flex 等按 service_tiers。
func (p *Pricing) tierMult(m *modelPrice, tier string) float64 {
	if tier == "priority" || tier == "fast" {
		if m.FastMult != nil {
			return *m.FastMult
		}
		return p.Tiers[tier].Mult
	}
	if t, ok := p.Tiers[tier]; ok && tier != "" {
		return t.Mult
	}
	return 1.0
}

// cost 返回 (美元, 是否长上下文, 速度档倍数, 价格表里有没有这个模型)。
// last = [输入, 其中缓存, 其中缓存写入, 输出, 其中推理, 合计]；输入含缓存，输出含推理。
// 运算顺序与原 Python 版一致，保证逐位相同。
func (p *Pricing) cost(model, tier string, last [6]int64) (float64, bool, float64, bool) {
	m, ok := p.resolve(model)
	if !ok {
		return 0, false, 1, false
	}
	inp, cached, cw, out := last[0], last[1], last[2], last[3]
	cached = min(cached, inp)
	cw = min(cw, inp-cached)
	uncached := inp - cached - cw
	long := (m.LongContext == nil || *m.LongContext) && inp > p.LongContext.Threshold
	mi, mc, mo := 1.0, 1.0, 1.0
	if long {
		mi, mc, mo = p.LongContext.InputMult, p.LongContext.CachedMult, p.LongContext.OutputMult
	}
	tm := p.tierMult(m, tier)
	// 每个乘积外包一层 float64()：禁止编译器把乘加合并成 FMA，保证和逐步舍入的结果逐位相同
	usd := (float64(float64(uncached)*m.Input*mi) +
		float64(float64(cw)*m.Input*m.cwMult()*mi) +
		float64(float64(cached)*m.CachedInput*mc) +
		float64(float64(out)*m.Output*mo)) / 1e6 * tm
	return usd, long, tm, true
}

// short 是不算长上下文、不算快速模式的基础价（与原版一样，这里缓存写入不做截断）。
func (p *Pricing) short(model string, last [6]int64) float64 {
	m, ok := p.resolve(model)
	if !ok {
		return 0
	}
	inp, cached, cw, out := last[0], min(last[1], last[0]), last[2], last[3]
	return (float64(float64(inp-cached-cw)*m.Input) + float64(float64(cw)*m.Input*m.cwMult()) +
		float64(float64(cached)*m.CachedInput) + float64(float64(out)*m.Output)) / 1e6
}
