#!/usr/bin/env python3
"""Mechanical citation check for a sweep output.

For every `path:line[-line]` citation in a markdown file: the file must exist under one of
the base dirs, the line must be inside the file, and at least one backticked identifier on
the same markdown line (other than the path) should appear within +/-6 lines of the cited
line. Rows failing the last test are listed as SOFT (need an eyeball), the first two as HARD.
"""
import os
import re
import sys

md = sys.argv[1]
bases = sys.argv[2:]
cite_re = re.compile(r'`?([A-Za-z0-9_./-]+\.(?:go|py|md|json|yaml|yml|txt|log|jsonl)):(\d+)(?:-(\d+))?`?')
ident_re = re.compile(r'`([A-Za-z_][A-Za-z0-9_.()*]*)`')

hard, soft, ok = [], [], 0
cache = {}


def lines_of(path):
    if path not in cache:
        with open(path, errors='replace') as fh:
            cache[path] = fh.read().split('\n')
    return cache[path]


def resolve(p):
    cands = [p] + [os.path.join(b, p) for b in bases]
    # also try stripping leading dirs progressively
    parts = p.split('/')
    for i in range(1, len(parts)):
        cands += [os.path.join(b, '/'.join(parts[i:])) for b in bases]
    for c in cands:
        if os.path.isfile(c):
            return c
    return None


with open(md) as fh:
    for ln_no, line in enumerate(fh, 1):
        cites = cite_re.findall(line)
        if not cites:
            continue
        idents = [i.strip('()*') for i in ident_re.findall(line)]
        for path, a, b in cites:
            a = int(a)
            b = int(b) if b else a
            real = resolve(path)
            if real is None:
                hard.append((ln_no, path, a, 'file not found'))
                continue
            src = lines_of(real)
            if a < 1 or b > len(src):
                hard.append((ln_no, path, a, f'line out of range (file has {len(src)})'))
                continue
            window = '\n'.join(src[max(0, a - 7):min(len(src), b + 6)])
            toks = [t for t in idents if t and t != path and not t.endswith(('.go', '.py', '.md', '.json'))]
            toks = [t.split('.')[-1] if '.' in t and not t.startswith('.') else t for t in toks]
            if toks and not any(t in window for t in toks):
                soft.append((ln_no, path, a, 'none of ' + ','.join(toks[:4]) + ' near line'))
            else:
                ok += 1

print(f'{md}: citations ok={ok} soft={len(soft)} hard={len(hard)}')
for h in hard:
    print('  HARD md:%d %s:%d %s' % h)
for s in soft[:40]:
    print('  SOFT md:%d %s:%d %s' % s)
