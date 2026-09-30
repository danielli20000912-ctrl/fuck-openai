"""校验 4：和 ccusage 在「同一批文件」上对账，把美元差距拆成 token 差距和价格差距。

控制变量：两边都只扫 ~/.codex/sessions + ~/.codex/archived_sessions，都含全部供应商，同一时区。
ccusage 的 inputTokens = 未命中缓存的输入，cacheReadTokens = 命中缓存的输入。"""
import json, subprocess, sys, os, collections
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import codex_cost as cc
from pathlib import Path

home = os.environ.get('CODEX_HOME', os.path.expanduser('~/.codex'))
tz = 'Asia/Shanghai'
ccu = json.loads(subprocess.run(['ccusage', 'codex', 'monthly', '--json', '--offline', '-z', tz],
                                capture_output=True, text=True, check=True).stdout)
out = subprocess.run([sys.executable, str(Path(cc.__file__)), '--json', '--quiet', '--provider', 'all', '--tz', tz,
                      '--root', f'{home}/sessions', '--root', f'{home}/archived_sessions'],
                     capture_output=True, text=True, check=True).stdout
rows = json.loads(out)
pricing = cc.Pricing(Path(cc.__file__).parent / 'prices.json')

mine = collections.defaultdict(lambda: collections.Counter())
for r in rows:
    k = (r['day'][:7], r['model'])
    m = mine[k]
    m['uncached'] += r['input'] - r['cached']; m['cached'] += r['cached']; m['output'] += r['output']
    m['usd'] += r['usd']; m['usd_short'] += r['usd_short']

def short_price(model, unc, cached, out):
    _, p = pricing.resolve(model)
    if not p: return 0.0
    return (unc * p['input'] + cached * p['cached_input'] + out * p['output']) / 1e6

print(f"{'月份':<8} {'模型':<20} {'ccusage 未缓存/缓存/输出 (M)':>32} {'本工具 未缓存/缓存/输出 (M)':>32} {'ccusage$':>9} {'ccu token×本价表$':>14} {'本工具短档$':>10} {'本工具$':>9}")
tot = collections.Counter()
keys = set()
for mon in ccu['monthly']:
    for model, v in mon['models'].items():
        keys.add((mon['month'], model))
keys |= set(mine.keys())
ccu_idx = {(mon['month'], model): v for mon in ccu['monthly'] for model, v in mon['models'].items()}
ccu_cost = {}
for mon in ccu['monthly']:
    ccu_cost[mon['month']] = mon['costUSD']
for k in sorted(keys):
    c = ccu_idx.get(k, {})
    m = mine.get(k, collections.Counter())
    cu = (c.get('inputTokens', 0), c.get('cacheReadTokens', 0), c.get('outputTokens', 0))
    mu = (m['uncached'], m['cached'], m['output'])
    cp = short_price(k[1], *cu)
    tot['ccu_tok_mine_price'] += cp; tot['mine_short'] += m['usd_short']; tot['mine'] += m['usd']
    f = lambda t: '/'.join(f'{x/1e6:.1f}' for x in t)
    print(f"{k[0]:<8} {k[1]:<20} {f(cu):>32} {f(mu):>32} {'':>9} {cp:>14.2f} {m['usd_short']:>10.2f} {m['usd']:>9.2f}")
print()
print('ccusage 自报总额 $', round(sum(ccu_cost.values()), 2), ccu_cost)
print('ccusage 的 token × 本价表短档 $', round(tot['ccu_tok_mine_price'], 2))
print('本工具 token × 本价表短档 $', round(tot['mine_short'], 2))
print('本工具最终（含长上下文+快速）$', round(tot['mine'], 2))
