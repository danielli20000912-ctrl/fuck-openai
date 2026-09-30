"""校验 3：Codex 自己的会话数据库（state_5.sqlite 的 threads 表）登记的会话，哪些在所有扫描目录里都找不到记录文件。
找不到的就是「这台机器上有过、但本工具算不到」的用量。tokens_used 对分叉会话是虚高的，只作量级参考。"""
import sys, os, sqlite3, collections, datetime
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import codex_cost as cc

home = os.environ.get('CODEX_HOME', os.path.expanduser('~/.codex'))
roots = [f'{home}/sessions', f'{home}/archived_sessions', *cc.load_config().get('extra_roots', [])]
parsed, missing, _ = cc.load_all(roots, cc.DEFAULT_CACHE, quiet=True)
have = {d['meta']['id'] for d in parsed.values() if d['meta']}
db = sqlite3.connect(f'file:{home}/state_5.sqlite?mode=ro', uri=True)
rows = db.execute("select id, created_at, originator, model_provider, tokens_used, thread_source, source from threads").fetchall()
lost = [r for r in rows if r[0] not in have]
print(f'数据库登记会话 {len(rows)}，有记录文件 {len(rows) - len(lost)}，找不到文件 {len(lost)}')
c = collections.Counter(); t = collections.Counter()
for r in lost:
    k = (datetime.datetime.fromtimestamp(r[1]).strftime('%Y-%m'), r[2] or '-', r[3], 'sub' if r[5] != 'user' else 'user')
    c[k] += 1; t[k] += r[4]
for k in sorted(c):
    print('  ', k, c[k], '个', f'tokens_used 合计 {t[k]/1e6:.1f}M')
# 反过来：有文件但数据库里没登记的
dbids = {r[0] for r in rows}
extra = [d['meta']['id'] for d in parsed.values() if d['meta'] and d['meta']['id'] not in dbids]
print(f'有记录文件但数据库没登记：{len(extra)} 个')
