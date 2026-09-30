"""用人造的会话记录验证去重和计价。金额都是手算的，价格取 prices.json。

    python3 -m unittest discover -s tests
"""
import datetime as dt
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
import codex_cost as cc  # noqa: E402

RESET = int(dt.datetime(2099, 1, 1, tzinfo=dt.timezone.utc).timestamp())


def ts(sec):
    return (dt.datetime(2026, 9, 1, tzinfo=dt.timezone.utc) + dt.timedelta(seconds=sec)).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def line(t, typ, payload):
    return json.dumps({"timestamp": ts(t), "type": typ, "payload": payload}, separators=(",", ":"))


def meta(t, sid, originator, **extra):
    return line(t, "session_meta", {"id": sid, "timestamp": ts(t), "originator": originator,
                                    "model_provider": "openai", "source": extra.pop("source", "vscode"), **extra})


def turn(t, model):
    return line(t, "turn_context", {"model": model})


def settings(t, tier):
    return line(t, "event_msg", {"type": "thread_settings_applied", "thread_settings": {"service_tier": tier}})


def usage(inp, cached, out):
    return {"input_tokens": inp, "cached_input_tokens": cached, "output_tokens": out,
            "reasoning_output_tokens": 0, "total_tokens": inp + out}


def tc(t, last, total, used=None):
    p = {"type": "token_count", "info": {"last_token_usage": usage(*last), "total_token_usage": usage(*total)}}
    if used is not None:
        p["rate_limits"] = {"limit_id": "codex", "plan_type": "pro", "secondary": None,
                            "primary": {"used_percent": used, "window_minutes": 10080, "resets_at": RESET}}
    return line(t, "event_msg", p)


# 父会话（桌面端，gpt-6-astra）：一次普通请求（写了两遍）、一次超过 272K 的请求、切到快速模式后一次请求
P1 = tc(10, (100_000, 60_000, 1_000), (100_000, 60_000, 1_000), used=10)
P2 = tc(20, (300_000, 200_000, 2_000), (400_000, 260_000, 3_000), used=12)
PARENT = [
    meta(0, "P", "Codex Desktop"),
    turn(1, "gpt-6-astra"),
    P1, P1,
    # 对话正文里出现 token_count 字样，不能被当成事件
    line(15, "response_item", {"type": "message", "content": '"type":"event_msg","payload":{"type":"token_count"'}),
    P2,
    settings(25, "priority"),
    tc(30, (50_000, 0, 0), (450_000, 260_000, 3_000), used=13),
]
# 子代理（命令行，从 P 分叉）：开头复制 P 的历史，然后自己用 gpt-6-sol 发一次请求
CHILD = [
    meta(100, "C", "codex-tui", forked_from_id="P",
         source={"subagent": {"thread_spawn": {"parent_thread_id": "P"}}}),
    meta(100, "P", "Codex Desktop"),
    # 复制来的父历史：内容一样，时间戳改成分叉时刻（真实日志就是这样）
    turn(100, "gpt-6-astra"), *(l.replace(ts(10), ts(100)).replace(ts(20), ts(100)) for l in (P1, P1, P2)),
    turn(110, "gpt-6-sol"),
    tc(120, (10_000, 0, 1_000), (410_000, 260_000, 4_000)),
]
# 无关会话（VSCode）：用量数字恰好和 P 的第一次请求完全一样，必须照算
UNRELATED = [meta(200, "U", "codex_vscode"), turn(201, "gpt-6-astra"),
             tc(210, (100_000, 60_000, 1_000), (100_000, 60_000, 1_000))]


class CodexCostTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        root = Path(cls.tmp.name) / "sessions"
        root.mkdir()
        for name, lines in (("P", PARENT), ("C", CHILD), ("U", UNRELATED)):
            (root / f"rollout-{name}.jsonl").write_text("\n".join(lines) + "\n")
        parsed, _, _ = cc.load_all([str(root)], Path(cls.tmp.name) / "cache.sqlite", quiet=True)
        cls.uniq, cls.stats = cc.dedup_events(parsed, cc.lineage_groups(parsed))
        pricing = cc.Pricing(ROOT / "prices.json")
        cls.rows = cc.build_rows(cls.uniq, parsed, pricing, dt.timezone.utc)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def total(self, **match):
        return round(sum(r["usd"] for r in self.rows if all(r[k] == v for k, v in match.items())), 6)

    def test_dedup(self):
        # P 3 次 + C 自己 1 次 + U 1 次；P 的重复写入和 C 复制来的历史都去掉
        self.assertEqual(len(self.rows), 5)
        self.assertEqual(self.stats["raw_events"], 4 + 4 + 1)

    def test_parent_cost(self):
        # 普通：(40K×10 + 60K×1 + 1K×50)/1M = 0.51
        # 长上下文：(100K×10×2 + 200K×1×2 + 2K×50×1.5)/1M = 2.55
        # 快速模式：50K×10/1M×2 = 1.00
        self.assertEqual(self.total(client="桌面端"), 4.06)
        self.assertEqual(sum(r["is_long"] for r in self.rows), 1)

    def test_child_counts_only_its_own_request(self):
        # gpt-6-sol：(10K×2 + 1K×10)/1M = 0.03
        self.assertEqual(self.total(client="命令行"), 0.03)

    def test_unrelated_session_not_merged(self):
        self.assertEqual(self.total(client="VSCode"), 0.51)
        self.assertEqual(self.stats["cross_group_same_usage"], 1)

    def test_tie_goes_to_parent(self):
        # 复制记录时间戳和原件完全相同时，也要归给父会话
        tmp = tempfile.TemporaryDirectory()
        root = Path(tmp.name)
        (root / "rollout-A-child.jsonl").write_text("\n".join(
            [meta(100, "C", "codex-tui", forked_from_id="P"), meta(100, "P", "Codex Desktop"),
             turn(1, "gpt-6-astra"), P1]) + "\n")
        (root / "rollout-B-parent.jsonl").write_text("\n".join(PARENT[:3]) + "\n")
        parsed, _, _ = cc.load_all([str(root)], root / "c.sqlite", quiet=True)
        uniq, _ = cc.dedup_events(parsed, cc.lineage_groups(parsed))
        self.assertEqual([os.path.basename(f) for f, _ in uniq], ["rollout-B-parent.jsonl"])
        tmp.cleanup()

    def test_quota_window(self):
        (w,) = cc.quota_windows(self.rows)
        # 10% → 13%，这之间的用量 = 2.55 + 1.00，整周 = 3.55 / 3 × 100
        self.assertEqual((w["u0"], w["u1"]), (10, 13))
        self.assertAlmostEqual(w["week_usd"], 3.55 / 3 * 100)


if __name__ == "__main__":
    unittest.main()
