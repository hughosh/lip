import re, sys, os, glob
DOC = '/Users/hugh/kek/lip/loop/run/client-portability/out/api-inventory.md'
GO = '/Users/hugh/kek/lip/go/'
SCR = '/private/tmp/claude-501/-Users-hugh-kek/12802585-9fa1-41d1-b8e5-0e40163305f9/scratchpad/api-inventory/'
base = {}
for d in ('harness/rest','harness/wsx','feed'):
    for f in glob.glob(GO+d+'/*.go'):
        if f.endswith('_test.go'): continue
        base.setdefault(os.path.basename(f), []).append(f)
base['params.go'] = [GO+'harness/cfg/params.go']
base['runtime.go'] = [GO+'cmd/harness/runtime.go']
def resolve(name, section):
    if name in ('openapi.yaml','asyncapi.yaml'): return SCR+name
    if name.startswith('/'): return name
    if name.startswith('notes/'): return '/Users/hugh/kek/lip/'+name
    if '/' in name: return GO+name
    c = base.get(name)
    if not c: return None
    if len(c)==1: return c[0]
    # ambiguous (wire.go): choose by section
    want = 'harness/wsx/' if section.startswith('3') else 'harness/rest/'
    for x in c:
        if want in x: return x
    return c[0]
file_re = re.compile(r'([A-Za-z0-9_./\-]+\.(?:go|yaml|md)):(\d[\d,\-]*)')
cont_re = re.compile(r'(?<![\w/.\-]):(\d[\d,\-]*)')
lines = open(DOC).read().split('\n')
section = '0'
cache = {}
def getlines(path):
    if path not in cache:
        cache[path] = open(path, errors='replace').read().split('\n')
    return cache[path]
out = []
bad = 0
for i, ln in enumerate(lines, 1):
    m = re.match(r'^#{2,3} (\d+(?:\.\d+)?)', ln)
    if m: section = m.group(1)
    # tokens in order of appearance
    toks = []
    for m in file_re.finditer(ln): toks.append((m.start(), 'f', m.group(1), m.group(2)))
    for m in cont_re.finditer(ln):
        # skip continuation tokens that are actually inside a file_re match
        if any(t[0] <= m.start() < t[0] + len(t[2]) + 1 + len(t[3]) for t in toks if t[1]=='f'): continue
        toks.append((m.start(), 'c', None, m.group(1)))
    toks.sort()
    cur = None
    for pos, kind, name, spec in toks:
        if kind == 'f': cur = name
        if cur is None: continue
        path = resolve(cur, section)
        if path is None or not os.path.exists(path):
            out.append(f"L{i} §{section} {cur}:{spec}  !! FILE NOT FOUND"); bad += 1; continue
        fl = getlines(path)
        for part in spec.split(','):
            part = part.strip('-')
            if not part: continue
            a, _, b = part.partition('-')
            a = int(a); b = int(b) if b else a
            if a < 1 or b > len(fl) or b < a:
                out.append(f"L{i} §{section} {cur}:{part}  !! OUT OF RANGE (file has {len(fl)} lines)"); bad += 1; continue
            first = fl[a-1].strip()[:95]
            last = fl[b-1].strip()[:95]
            tag = f"{os.path.relpath(path, GO) if path.startswith(GO) else os.path.basename(path)}:{part}"
            if a == b:
                out.append(f"L{i} §{section} {tag} | {first}")
            else:
                out.append(f"L{i} §{section} {tag} | {first}  ...  {last}")
print("total cites:", len(out), " hard errors:", bad)
open(SCR+'cites_report.txt','w').write('\n'.join(out))
