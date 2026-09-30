"""校验 1：累计值与「本次用量」是否自洽。

在每个文件里按事件顺序走，累计值（输入/缓存/输出三项）变小的地方视为计数器重起（上下文压缩后会这样），
在每一段里检查：Σ(累计值变化的那些事件的本次用量) == 段末累计 − 段首累计 + 段首本次用量。
另外统计「累计值没变、本次用量却不为零」的事件——这类事件算不算钱，不同工具处理不同。"""
import sys, os
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import codex_cost as cc

home = os.environ.get('CODEX_HOME', os.path.expanduser('~/.codex'))
roots = [f'{home}/sessions', f'{home}/archived_sessions', *cc.load_config().get('extra_roots', [])]
parsed, missing, fresh = cc.load_all(roots, cc.DEFAULT_CACHE, quiet=True)
F = (0, 1, 3)  # input, cached, output
ok_files = bad_files = 0
seg_total = seg_bad = 0
stale_nonzero = 0
stale_tokens = [0, 0, 0]
bad = []
for f, d in parsed.items():
    ev = d['events']
    if not ev:
        continue
    segs = [[]]
    prev = None
    for e in ev:
        tot = [e[cc.E_TOTAL][i] for i in F]
        if prev is not None and any(t < p for t, p in zip(tot, prev)):
            segs.append([])
        if prev is not None and tot == prev:
            last = [e[cc.E_LAST][i] for i in F]
            if any(last):
                stale_nonzero += 1
                for i in range(3):
                    stale_tokens[i] += last[i]
            continue
        segs[-1].append(e)
        prev = tot
    file_ok = True
    for s in segs:
        if not s:
            continue
        seg_total += 1
        got = [sum(e[cc.E_LAST][i] for e in s) for i in F]
        exp = [s[-1][cc.E_TOTAL][i] - s[0][cc.E_TOTAL][i] + s[0][cc.E_LAST][i] for i in F]
        if got != exp:
            seg_bad += 1
            file_ok = False
            bad.append((os.path.basename(f)[:62], got, exp))
    ok_files += file_ok
    bad_files += (not file_ok)
print(f'文件：自洽 {ok_files}，不自洽 {bad_files}；分段 {seg_total}，不自洽段 {seg_bad}')
print(f'累计值没变但本次用量非零的事件：{stale_nonzero} 条，输入/缓存/输出 = {stale_tokens}')
for b in bad[:10]:
    print('  ', b)
