#!/usr/bin/env python3
"""把本机 Codex（桌面端 / VSCode / 命令行 / Chrome 侧栏 / iPhone 远程）的用量按 OpenAI API 价折算成美元。

数据来源：Codex 写在本地的会话记录 rollout-*.jsonl（三端共用 CODEX_HOME，默认 ~/.codex），
外加你手动搬走的归档目录（--root 追加，或写进 config.json）。

和 ccusage 等工具的主要差别（每一条都在 README 里有实测数字）：
  1. 子代理 / 分叉会话的文件开头会原样复制父会话的全部历史（含用量记录），
     只在有父子关系的会话之间去重，无关会话之间绝不合并。
  2. 同一次请求的用量事件经常连写两遍，按累计值去重。
  3. 模型认不出来时不套默认价，单列「未知模型」行，按 $0 计并报数。
  4. 长上下文按 OpenAI 规则：单次请求输入（含缓存命中）超过 272K，整单输入×2、缓存×2、输出×1.5。
  5. 快速模式（service_tier=priority）逐请求识别，按 prices.json 里的倍数计价。

只用 Python 标准库。
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import sqlite3
import sys
import time
import zlib
from collections import defaultdict
from pathlib import Path

HERE = Path(__file__).resolve().parent
CACHE_VERSION = 3
DEFAULT_CACHE = HERE / ".cache" / "cache.sqlite"  # 放外置盘项目目录，不占内置盘

# 行首 250 字节里认事件类型；不能只搜关键字，否则对话正文里提到 token_count 也会被当成事件
RE_SESSION_META = re.compile(rb'^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"session_meta"')
RE_TURN_CONTEXT = re.compile(rb'^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"turn_context"')
RE_TOKEN_COUNT = re.compile(rb'^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"event_msg","payload":\{"type":"token_count"')
RE_SETTINGS = re.compile(rb'^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"event_msg","payload":\{"type":"thread_settings_applied"')
RE_USAGE_RECORD = re.compile(rb'^\{"timestamp":"[^"]*",(?:"ordinal":\d+,)?"type":"token_usage_record"')

CLIENT_LABELS = {
    "Codex Desktop": "桌面端",
    "codex_work_desktop": "桌面端(Work)",
    "codex_vscode": "VSCode",
    "codex-tui": "命令行",
    "codex_cli_rs": "命令行",
    "codex_exec": "命令行",
    "codex-chrome-extension-sidepanel": "Chrome 侧栏",
    "codex_chatgpt_ios_remote": "iPhone 远程",
}

USAGE_FIELDS = ("input_tokens", "cached_input_tokens", "cache_write_input_tokens",
                "output_tokens", "reasoning_output_tokens", "total_tokens")


# ---------------------------------------------------------------- 解析单个文件

def _ms(ts: str) -> int:
    # "2026-09-29T21:08:29.525Z"
    return int(dt.datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp() * 1000)


def _usage(u: dict | None) -> list[int]:
    u = u or {}
    return [int(u.get(k) or 0) for k in USAGE_FIELDS]


def _rate_limits(rl: dict | None):
    if not rl:
        return None
    p = rl.get("primary") or {}
    s = rl.get("secondary") or {}
    return [rl.get("plan_type"), rl.get("limit_id"),
            p.get("used_percent"), p.get("window_minutes"), p.get("resets_at"),
            s.get("used_percent"), s.get("window_minutes"), s.get("resets_at")]


def parse_file(path: str) -> dict:
    """返回 {meta, other_ids, events, records}。events 按文件顺序。"""
    meta = None
    other_ids: set[str] = set()
    events = []
    records = []
    model = None
    tier = None
    provider = None
    with open(path, "rb") as fh:
        for line in fh:
            head = line[:250]
            if RE_TOKEN_COUNT.match(head):
                o = json.loads(line)
                info = o["payload"].get("info")
                if not info:
                    continue
                events.append([
                    _ms(o["timestamp"]), model, tier, provider,
                    *_usage(info.get("last_token_usage")),
                    *_usage(info.get("total_token_usage")),
                    _rate_limits(o["payload"].get("rate_limits")),
                ])
            elif RE_TURN_CONTEXT.match(head):
                p = json.loads(line)["payload"]
                if p.get("model"):
                    model = p["model"]
                if p.get("service_tier") is not None:
                    tier = p["service_tier"]
            elif RE_SETTINGS.match(head):
                ts = json.loads(line)["payload"].get("thread_settings") or {}
                if ts.get("model"):
                    model = ts["model"]
                if "service_tier" in ts:
                    tier = ts["service_tier"]
                if ts.get("model_provider_id"):
                    provider = ts["model_provider_id"]
            elif RE_USAGE_RECORD.match(head):
                o = json.loads(line)
                p = o["payload"]
                records.append([p.get("response_id"), _ms(o["timestamp"]), p.get("thread_id"),
                                *_usage(p.get("usage"))])
            elif RE_SESSION_META.match(head):
                o = json.loads(line)
                p = o["payload"]
                if meta is None:
                    src = p.get("source")
                    parent = None
                    sub_kind = None
                    if isinstance(src, dict) and "subagent" in src:
                        sa = src["subagent"]
                        if isinstance(sa, dict):
                            if "thread_spawn" in sa:
                                sub_kind = "spawn"
                                parent = (sa["thread_spawn"] or {}).get("parent_thread_id")
                            else:
                                sub_kind = str(sa.get("other") or next(iter(sa), "other"))
                        else:
                            sub_kind = str(sa)
                    meta = {
                        "id": p.get("id"),
                        "ts": _ms(p.get("timestamp") or o["timestamp"]),
                        "originator": p.get("originator"),
                        "source": src if isinstance(src, str) else "subagent",
                        "subagent": sub_kind,
                        "parent_id": parent,
                        "forked_from_id": p.get("forked_from_id"),
                        "provider": p.get("model_provider"),
                        "cli_version": p.get("cli_version"),
                        "account": p.get("creator_account_id"),
                    }
                    provider = p.get("model_provider")
                elif p.get("id") and p.get("id") != meta["id"]:
                    other_ids.add(p["id"])
    return {"meta": meta, "other_ids": sorted(other_ids), "events": events, "records": records}


# ---------------------------------------------------------------- 缓存（归档文件不再变化，第二次运行只读新文件）

class Cache:
    def __init__(self, path: Path):
        path.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(str(path))
        self.db.execute("create table if not exists files(path text primary key, size int, mtime int, ver int, data blob)")

    def get(self, path, size, mtime):
        row = self.db.execute("select size, mtime, ver, data from files where path=?", (path,)).fetchone()
        if row and row[0] == size and row[1] == mtime and row[2] == CACHE_VERSION:
            return json.loads(zlib.decompress(row[3]))
        return None

    def put(self, path, size, mtime, data):
        blob = zlib.compress(json.dumps(data, separators=(",", ":")).encode(), 6)
        self.db.execute("insert or replace into files values(?,?,?,?,?)", (path, size, mtime, CACHE_VERSION, blob))

    def commit(self):
        self.db.commit()


def find_files(roots: list[str]) -> tuple[list[str], list[str]]:
    files, missing = [], []
    for r in roots:
        if not os.path.isdir(r):
            missing.append(r)
            continue
        for dp, _, fn in os.walk(r):
            for f in fn:
                if f.startswith("rollout-") and f.endswith(".jsonl"):
                    files.append(os.path.join(dp, f))
    return sorted(files), missing


def load_all(roots, cache_path, quiet=False):
    files, missing = find_files(roots)
    cache = Cache(cache_path)
    parsed = {}
    t0 = time.time()
    fresh = 0
    for i, f in enumerate(files):
        st = os.stat(f)
        mtime = int(st.st_mtime)
        data = cache.get(f, st.st_size, mtime)
        if data is None:
            data = parse_file(f)
            cache.put(f, st.st_size, mtime, data)
            fresh += 1
            if fresh % 50 == 0:
                cache.commit()
        parsed[f] = data
        if not quiet and fresh and i % 100 == 0:
            print(f"  读取 {i + 1}/{len(files)} 个文件，{time.time() - t0:.0f}s", file=sys.stderr)
    cache.commit()
    return parsed, missing, fresh


# ---------------------------------------------------------------- 去重

def lineage_groups(parsed: dict) -> dict[str, int]:
    """并查集：自己的 id、父会话 id、分叉来源 id、文件里出现过的其他会话 id 全部连起来。"""
    parent: dict[str, str] = {}

    def find(x):
        parent.setdefault(x, x)
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a, b):
        ra, rb = find(a), find(b)
        if ra != rb:
            parent[ra] = rb

    for f, d in parsed.items():
        m = d["meta"]
        me = m["id"] if m and m.get("id") else "file:" + f
        find(me)
        if m:
            for other in (m.get("parent_id"), m.get("forked_from_id")):
                if other:
                    union(me, other)
        for other in d["other_ids"]:
            union(me, other)
    out = {}
    for f, d in parsed.items():
        m = d["meta"]
        me = m["id"] if m and m.get("id") else "file:" + f
        out[f] = find(me)
    return out


# 事件在内存里的下标
E_TS, E_MODEL, E_TIER, E_PROV = 0, 1, 2, 3
E_LAST = slice(4, 10)
E_TOTAL = slice(10, 16)
E_RL = 16


def dedup_events(parsed: dict, groups: dict[str, str]):
    """在同一血缘组内按（累计用量 + 本次用量）去重，保留时间最早的那一份。

    返回 (unique_events, stats)。unique_events 每项是 (file, event)。
    """
    best: dict[tuple, tuple[str, list]] = {}
    rank: dict[tuple, tuple] = {}
    stats = defaultdict(int)
    for f, d in parsed.items():
        g = groups[f]
        m = d["meta"] or {}
        is_fork = 1 if (m.get("forked_from_id") or m.get("parent_id")) else 0
        for e in d["events"]:
            stats["raw_events"] += 1
            key = (g, *e[E_TOTAL], *e[E_LAST])
            # 时间最早的那份是原件；时间相同时归给非分叉的会话，再按会话创建先后
            r = (e[E_TS], is_fork, m.get("ts") or 0, f)
            if key not in best:
                best[key], rank[key] = (f, e), r
            else:
                stats["dup_events"] += 1
                if r < rank[key]:
                    best[key], rank[key] = (f, e), r
    uniq = list(best.values())
    stats["unique_events"] = len(uniq)

    # 质量检查 A：不同血缘组之间出现完全相同的（累计+本次）用量 → 可能有没连上的分叉在重复计数
    seen_groups: dict[tuple, str] = {}
    for (g, *k) in best.keys():
        k = tuple(k)
        if k[5] < 1000:  # 极小的请求撞车没有意义
            continue
        if k in seen_groups and seen_groups[k] != g:
            stats["cross_group_same_usage"] += 1
        else:
            seen_groups[k] = g

    # 质量检查 B：父会话文件已不在任何目录里，子会话开头复制来的父历史是唯一记录（时间戳是分叉时刻）
    have_ids = {d["meta"]["id"] for d in parsed.values() if d["meta"]}
    for f, e in uniq:
        m = parsed[f]["meta"]
        if not m:
            continue
        parent = m.get("forked_from_id") or m.get("parent_id")
        if parent and parent not in have_ids and e[E_TS] - m["ts"] < 5000:
            stats["replay_only_events"] += 1
            stats["replay_only_tokens"] += e[4] + e[7]
    return uniq, stats


# ---------------------------------------------------------------- 计价

class Pricing:
    def __init__(self, path: Path):
        self.raw = json.loads(path.read_text())
        self.models = self.raw["models"]
        self.aliases = self.raw.get("aliases", {})
        self.lc = self.raw["long_context"]
        self.tiers = self.raw.get("service_tiers", {})
        self.credits_per_usd = float(self.raw.get("rate_card", {}).get("credits_per_usd", 25))

    def resolve(self, model: str | None):
        if not model:
            return None, None
        name = self.aliases.get(model, model)
        return name, self.models.get(name)

    def cost(self, model, tier, last):
        """last = [input, cached, cache_write, output, reasoning, total]；返回 (usd, is_long, tier_mult, known)."""
        name, p = self.resolve(model)
        if p is None:
            return 0.0, False, 1.0, False
        inp, cached, cw, out = last[0], last[1], last[2], last[3]
        cached = min(cached, inp)
        cw = min(cw, inp - cached)
        uncached = inp - cached - cw
        is_long = bool(p.get("long_context", True)) and inp > self.lc["threshold_input_tokens"]
        mi = self.lc["input_mult"] if is_long else 1.0
        mc = self.lc["cached_mult"] if is_long else 1.0
        mo = self.lc["output_mult"] if is_long else 1.0
        tm = 1.0
        if tier in ("priority", "fast"):
            tm = float(p.get("fast_mult", self.tiers[tier]["mult"]))
        elif tier and tier in self.tiers:
            tm = float(self.tiers[tier]["mult"])
        usd = (uncached * p["input"] * mi
               + cw * p["input"] * p.get("cache_write_mult", 1.25) * mi
               + cached * p["cached_input"] * mc
               + out * p["output"] * mo) / 1e6 * tm
        return usd, is_long, tm, True


# ---------------------------------------------------------------- 汇总

def client_label(originator):
    if not originator:
        return "未知客户端"
    return CLIENT_LABELS.get(originator, originator)


def local_day(ms, tz):
    return dt.datetime.fromtimestamp(ms / 1000, tz).strftime("%Y-%m-%d")


def build_rows(uniq, parsed, pricing, tz):
    rows = []
    for f, e in uniq:
        m = parsed[f]["meta"] or {}
        last = e[E_LAST]
        usd, is_long, tm, known = pricing.cost(e[E_MODEL], e[E_TIER], last)
        # usd_short = 不算长上下文、不算快速模式的基础价，用来单列加价部分
        name, p = pricing.resolve(e[E_MODEL])
        if p:
            inp, cached, cw, out = last[0], min(last[1], last[0]), last[2], last[3]
            short = ((inp - cached - cw) * p["input"] + cw * p["input"] * p.get("cache_write_mult", 1.25)
                     + cached * p["cached_input"] + out * p["output"]) / 1e6
        else:
            short = 0.0
        rows.append({
            "ts": e[E_TS],
            "day": local_day(e[E_TS], tz),
            "client": client_label(m.get("originator")),
            "originator": m.get("originator") or "",
            "subagent": m.get("subagent") or "",
            "provider": e[E_PROV] or m.get("provider") or "",
            "model": e[E_MODEL] or "(未知)",
            "tier": e[E_TIER] or "(旧版未记录,按标准)",
            "input": last[0], "cached": min(last[1], last[0]), "cache_write": last[2],
            "output": last[3], "reasoning": last[4],
            "usd": usd, "usd_short": short, "is_long": is_long, "known": known,
            # 费率卡点数：订阅额度按官方 Codex 费率卡扣，费率卡 = API 短档价 × credits_per_usd，快速模式另乘倍数
            "credits": short * pricing.credits_per_usd * (tm if e[E_TIER] in ("priority", "fast") else 1.0),
            "file": f,
            "rl": e[E_RL],
        })
    return rows


def agg(rows, key):
    out = defaultdict(lambda: defaultdict(float))
    for r in rows:
        k = key(r)
        a = out[k]
        a["req"] += 1
        for f in ("input", "cached", "output", "reasoning", "usd", "usd_short"):
            a[f] += r[f]
        if r["is_long"]:
            a["long_req"] += 1
        if not r["known"]:
            a["unknown_tokens"] += r["input"] + r["output"]
    return out


def fmt_tok(n):
    n = float(n)
    if n >= 1e9:
        return f"{n / 1e9:.2f}B"
    if n >= 1e6:
        return f"{n / 1e6:.1f}M"
    if n >= 1e3:
        return f"{n / 1e3:.1f}K"
    return f"{n:.0f}"


def print_table(title, data, key_name, sort_by="usd", limit=None):
    print(f"\n## {title}")
    hdr = f"{key_name:<22} {'请求数':>7} {'输入':>9} {'其中缓存':>9} {'输出':>8} {'API价值$':>11} {'其中长上下文+快速加价$':>14}"
    print(hdr)
    print("-" * len(hdr.encode("gbk", "ignore")))
    items = sorted(data.items(), key=lambda kv: -kv[1][sort_by]) if sort_by else sorted(data.items())
    if limit:
        items = items[:limit]
    tot = defaultdict(float)
    for k, a in items:
        print(f"{str(k):<22} {int(a['req']):>7} {fmt_tok(a['input']):>9} {fmt_tok(a['cached']):>9} "
              f"{fmt_tok(a['output']):>8} {a['usd']:>11,.2f} {a['usd'] - a['usd_short']:>14,.2f}")
        for f in a:
            tot[f] += a[f]
    print(f"{'合计':<22} {int(tot['req']):>7} {fmt_tok(tot['input']):>9} {fmt_tok(tot['cached']):>9} "
          f"{fmt_tok(tot['output']):>8} {tot['usd']:>11,.2f} {tot['usd'] - tot['usd_short']:>14,.2f}")


def db_coverage(parsed, codex_home):
    """Codex 自己的会话库里登记了、但所有扫描目录都找不到记录文件的会话（这部分用量算不到）。"""
    db_path = os.path.join(codex_home, "state_5.sqlite")
    if not os.path.exists(db_path):
        return None
    have = {d["meta"]["id"] for d in parsed.values() if d["meta"]}
    try:
        db = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        rows = db.execute("select id, created_at, thread_source, tokens_used from threads").fetchall()
    except sqlite3.Error:
        return None
    lost = [r for r in rows if r[0] not in have]
    days = sorted(dt.datetime.fromtimestamp(r[1]).strftime("%Y-%m-%d") for r in lost)
    return {
        "threads": len(rows), "lost": len(lost),
        "lost_sub": sum(1 for r in lost if r[2] != "user"),
        "lost_user_tokens": sum(r[3] for r in lost if r[2] == "user"),
        "span": (days[0], days[-1]) if days else None,
    }


# ---------------------------------------------------------------- 周额度反推

def _weekly(rl):
    """从一张额度快照里取「codex 主桶的周窗口」：(已用%, 重置时间, 套餐)。取不到返回 None。"""
    if not rl or rl[1] != "codex":
        return None
    for used, win, reset in ((rl[2], rl[3], rl[4]), (rl[5], rl[6], rl[7])):
        if win == 10080 and used is not None and reset:
            return float(used), int(reset), rl[0]
    return None


def quota_windows(rows):
    """把请求按「周额度窗口」分组，反推每个窗口整周额度值多少 API 美元。

    每条用量记录都附带服务器当时的周额度已用百分比。一个会话只属于一个账号，所以每条请求归到
    它所在会话里最近一张 codex 主桶快照的窗口（不同账号的重置时间不同，靠这个区分）。
    窗口内：第一张快照之后、最后一张快照为止的用量 ÷ 百分比涨幅 × 100 = 整周额度的价值。
    """
    by_file = defaultdict(list)
    for r in rows:
        by_file[r["file"]].append(r)
    win = {}
    for f, rs in by_file.items():
        rs.sort(key=lambda r: r["ts"])
        cur = None
        for r in rs:
            w = _weekly(r["rl"])
            if w:
                cur = round(w[1] / 3600)  # 重置时间有几秒抖动，按小时归并
                W = win.setdefault(cur, {"reset": w[1], "plan": w[2], "snaps": [], "reqs": []})
                W["snaps"].append((r["ts"], w[0]))
            if cur is not None:
                win[cur]["reqs"].append(r)
    out = []
    for k, W in sorted(win.items()):
        snaps = sorted(W["snaps"])
        (t0, u0), (t1, u1) = snaps[0], snaps[-1]
        seg = [r for r in W["reqs"] if t0 < r["ts"] <= t1]
        usd = sum(r["usd"] for r in seg)
        credits = sum(r["credits"] for r in seg)
        models = defaultdict(float)
        for r in seg:
            models[r["model"]] += r["usd"]
        du = u1 - u0
        out.append({
            "reset": W["reset"], "plan": W["plan"], "snaps": len(snaps),
            "t0": t0, "t1": t1, "u0": u0, "u1": u1, "du": du,
            "usd": usd, "credits": credits,
            # 百分比是整数，涨幅两端各有 ±0.5 的取整误差
            "week_usd": usd / du * 100 if du > 0 else None,
            "week_usd_lo": usd / (du + 1) * 100 if du > 0 else None,
            "week_usd_hi": usd / (du - 1) * 100 if du > 1 else None,
            "week_credits": credits / du * 100 if du > 0 else None,
            "models": sorted(models.items(), key=lambda kv: -kv[1]),
            "last_used": u1, "last_ts": t1,
        })
    return out


def print_quota(rows, tz, min_rise=5):
    ws = quota_windows(rows)
    now = time.time()

    def t(ms_or_s, ms=True):
        return dt.datetime.fromtimestamp(ms_or_s / 1000 if ms else ms_or_s, tz).strftime("%m-%d %H:%M")

    def mix(w):
        tot = sum(v for _, v in w["models"]) or 1
        return "，".join(f"{m} {v / tot:.0%}" for m, v in w["models"][:2])

    cur = [w for w in ws if w["reset"] > now]
    print("\n## 本周（还没重置的窗口）")
    if not cur:
        print("本机日志里没有本周的额度读数（这周还没在本机用过 Codex）。")
    for w in cur:
        print(f"套餐 {w['plan'] or '?'}，{t(w['reset'], ms=False)} 重置；最近读数 {t(w['last_ts'])} 已用 {w['last_used']:.0f}%")
        if w["du"] >= min_rise:
            left = w["week_usd"] * (100 - w["last_used"]) / 100
            print(f"  本窗口在本机用掉 API 价值 ${w['usd']:,.2f}，额度涨了 {w['du']:.0f}% → "
                  f"整周额度约值 ${w['week_usd']:,.0f}（取整误差范围 ${w['week_usd_lo']:,.0f}–${w['week_usd_hi'] or float('nan'):,.0f}），"
                  f"剩下的 {100 - w['last_used']:.0f}% 约值 ${left:,.0f}")
        else:
            print(f"  本窗口额度只涨了 {w['du']:.0f}%，不到 {min_rise}%，整数百分比的取整误差太大，先不推算")

    print(f"\n## 历史各周：整周额度折合 API 价值（涨幅 ≥ {min_rise}% 的窗口才推算）")
    hdr = f"{'重置时间':<12} {'套餐':<5} {'已用% 起→止':>11} {'这段API价值$':>12} {'整周额度≈$':>11} {'取整误差范围$':>16} {'整周≈费率卡点':>13}  主力模型"
    print(hdr)
    for w in ws:
        if w["du"] < min_rise:
            continue
        rng = f"{w['week_usd_lo']:,.0f}–{w['week_usd_hi']:,.0f}" if w["week_usd_hi"] else "-"
        print(f"{t(w['reset'], ms=False):<12} {str(w['plan'] or '?'):<5} {w['u0']:>4.0f}→{w['u1']:<5.0f} "
              f"{w['usd']:>12,.2f} {w['week_usd']:>11,.0f} {rng:>16} {w['week_credits']:>13,.0f}  {mix(w)}")
    print("\n说明：同一账号在别的电脑/手机上用、或多人共用一个账号时，那部分用量本机看不到，推算值会偏低。"
          "整周价值随模型组合变化（同样 1% 额度，不同模型折合的 API 价差很多），比较不同周时看同类模型为主的窗口。")


# ---------------------------------------------------------------- 主程序

def load_config():
    p = HERE / "config.json"
    if p.exists():
        return json.loads(p.read_text())
    return {}


def main(argv=None):
    cfg = load_config()
    codex_home = os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))
    ap = argparse.ArgumentParser(
        description="Codex 用量按 OpenAI API 价折算（桌面端 + VSCode + 命令行），并反推周额度值多少钱")
    ap.add_argument("mode", nargs="?", choices=["usage", "quota"], default="usage",
                    help="usage = 用量折合 API 价值（默认）；quota = 本周额度实时价值 + 历史各周整周额度价值")
    ap.add_argument("--root", action="append", default=None,
                    help="会话目录，可多次指定；默认 CODEX_HOME/sessions、archived_sessions 加 config.json 里的 extra_roots")
    ap.add_argument("--since", help="起始日期 YYYY-MM-DD（含）")
    ap.add_argument("--until", help="结束日期 YYYY-MM-DD（含）")
    ap.add_argument("--by", choices=["day", "week", "month"], default="month")
    ap.add_argument("--provider", default="openai",
                    help="只统计这个供应商（openai = 用 ChatGPT 账号登录的官方订阅）；all = 全部")
    ap.add_argument("--tz", default=cfg.get("timezone", "Asia/Shanghai"))
    ap.add_argument("--prices", default=str(HERE / "prices.json"))
    ap.add_argument("--cache", default=str(DEFAULT_CACHE))
    ap.add_argument("--watch", type=int, metavar="秒", help="quota 模式下每隔几秒刷新一次")
    ap.add_argument("--json", action="store_true", help="输出逐请求明细 JSON（给别的脚本用）")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args(argv)
    if args.watch:
        while True:
            print("\033[2J\033[H", end="")
            run(args, cfg, codex_home)
            time.sleep(args.watch)
    run(args, cfg, codex_home)


def run(args, cfg, codex_home):
    try:
        from zoneinfo import ZoneInfo
        tz = ZoneInfo(args.tz)
    except Exception:
        tz = dt.timezone(dt.timedelta(hours=8))

    roots = args.root or [os.path.join(codex_home, "sessions"), os.path.join(codex_home, "archived_sessions"),
                          *cfg.get("extra_roots", [])]
    t0 = time.time()
    parsed, missing, fresh = load_all(roots, Path(args.cache), quiet=args.quiet or bool(args.watch))
    groups = lineage_groups(parsed)
    uniq, st = dedup_events(parsed, groups)
    pricing = Pricing(Path(args.prices))
    rows = build_rows(uniq, parsed, pricing, tz)

    if args.since:
        rows = [r for r in rows if r["day"] >= args.since]
    if args.until:
        rows = [r for r in rows if r["day"] <= args.until]
    all_rows = rows
    if args.provider != "all":
        rows = [r for r in rows if r["provider"] == args.provider]

    if args.json:
        json.dump([{k: v for k, v in r.items() if k not in ("file", "rl")} | {"file": os.path.basename(r["file"])}
                   for r in sorted(rows, key=lambda r: r["ts"])], sys.stdout, ensure_ascii=False)
        return

    if args.mode == "quota":
        print(f"# Codex 周额度价值（供应商={args.provider}，时区 {args.tz}，{dt.datetime.now(tz):%Y-%m-%d %H:%M:%S}）")
        print(f"价格表 {os.path.basename(args.prices)}（核对日期 {pricing.raw.get('checked')}）；"
              f"文件 {len(parsed)} 个，去重后请求 {st['unique_events']:,} 次，耗时 {time.time() - t0:.1f}s"
              + (f"；⚠ 未挂载/不存在：{', '.join(missing)}" if missing else ""))
        print_quota(rows, tz)
        return

    def period(r):
        if args.by == "day":
            return r["day"]
        if args.by == "month":
            return r["day"][:7]
        d = dt.date.fromisoformat(r["day"])
        return (d - dt.timedelta(days=d.weekday())).isoformat() + " 周"

    span = f"{min(r['day'] for r in rows)} → {max(r['day'] for r in rows)}" if rows else "无数据"
    print(f"# Codex 用量按 API 价折算（{span}，供应商={args.provider}，时区 {args.tz}）")
    print(f"价格表：{args.prices}（核对日期 {pricing.raw.get('checked')}；所有历史用量按这一张现价表计）")
    print("扫描目录：")
    for r in roots:
        print(f"  {'✗ 不存在/未挂载' if r in missing else '✓'} {r}")
    print(f"文件 {len(parsed)} 个（本次新解析 {fresh}），用量事件 {st['raw_events']:,} 条，"
          f"去重后 {st['unique_events']:,} 条（去掉子代理复制的父会话历史和重复写入 {st['dup_events']:,} 条），"
          f"耗时 {time.time() - t0:.1f}s")
    print(f"数据质量：父会话文件已丢失、只能用子会话复制的历史补回的用量 {st['replay_only_events']:,} 条"
          f"（{fmt_tok(st['replay_only_tokens'])} token，日期记在分叉当天）；"
          f"不同会话间用量完全相同的 {st['cross_group_same_usage']:,} 条（多说明有没识别出的分叉）")
    cov = db_coverage(parsed, codex_home)
    if cov and cov["lost"]:
        print(f"          Codex 会话库登记 {cov['threads']} 个会话，其中 {cov['lost']} 个在所有目录里都找不到记录文件"
              f"（{cov['span'][0]} → {cov['span'][1]}；子代理 {cov['lost_sub']} 个，主会话 {cov['lost'] - cov['lost_sub']} 个、"
              f"会话库记的累计 {fmt_tok(cov['lost_user_tokens'])} token）——这部分算不到")

    print_table(f"按{ {'day': '日', 'week': '周', 'month': '月'}[args.by] }", agg(rows, period), "时间", sort_by=None)
    print_table("按客户端", agg(rows, lambda r: r["client"]), "客户端")
    print_table("按客户端（主会话 / 子代理分开）",
                agg(rows, lambda r: f"{r['client']}·{'子代理' if r['subagent'] else '主会话'}"), "客户端")
    print_table("按模型", agg(rows, lambda r: r["model"]), "模型")
    print_table("按速度档（priority = 快速模式，API 价×2）", agg(rows, lambda r: r["tier"]), "速度档")
    if args.provider != "all":
        print_table("全部供应商（对照：openai 以外的是第三方供应商）", agg(all_rows, lambda r: r["provider"]), "供应商")

    unk = defaultdict(float)
    for r in rows:
        if not r["known"]:
            unk[r["model"]] += r["input"] + r["output"]
    if unk:
        print("\n⚠ 价格表里没有的模型（按 $0 计，需要补价）：" +
              "，".join(f"{k} {fmt_tok(v)} token" for k, v in sorted(unk.items(), key=lambda kv: -kv[1])))


if __name__ == "__main__":
    main()
