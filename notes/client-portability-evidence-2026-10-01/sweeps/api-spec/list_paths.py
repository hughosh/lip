import yaml, sys, json
spec = yaml.safe_load(open('openapi.yaml'))
paths = spec['paths']
rows = []
for p, item in paths.items():
    for m, op in item.items():
        if m in ('get','post','put','delete','patch'):
            tags = ','.join(op.get('tags', []))
            params = [x.get('name') if 'name' in x else x.get('$ref','').split('/')[-1] for x in op.get('parameters', [])]
            rows.append((tags, p, m.upper(), op.get('operationId',''), op.get('summary',''), params, bool(op.get('security')), op.get('deprecated', False)))
rows.sort()
print("TOTAL paths:", len(paths), "operations:", len(rows))
cur = None
for r in rows:
    if r[0] != cur:
        cur = r[0]
        print("\n## tag:", cur)
    print(f"{r[2]:6} {r[1]}  [{r[3]}] {r[4]}  dep={r[7]} sec={r[6]}")
