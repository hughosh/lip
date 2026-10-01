import yaml, sys, json
spec = yaml.safe_load(open('openapi.yaml'))
comps = spec['components']
def resolve(node, depth=0, maxd=4, seen=None):
    seen = seen or set()
    if isinstance(node, dict):
        if '$ref' in node:
            name = node['$ref'].split('/')[-1]
            if name in seen or depth >= maxd:
                return {'$ref': name}
            tgt = node['$ref'].split('/')[1:]
            cur = spec
            for t in tgt: cur = cur[t]
            return {'$ref': name, **resolve(cur, depth+1, maxd, seen | {name})}
        return {k: resolve(v, depth, maxd, seen) for k, v in node.items() if k not in ('example','examples')}
    if isinstance(node, list):
        return [resolve(x, depth, maxd, seen) for x in node]
    return node

def schema(name, maxd=3):
    print(f"### schema {name}")
    print(yaml.safe_dump(resolve(comps['schemas'][name], 0, maxd), sort_keys=False, width=160))

if __name__ == '__main__':
    for a in sys.argv[1:]:
        if a.startswith('S:'):
            schema(a[2:])
        else:
            m, p = a.split(' ',1)
            op = spec['paths'][p][m.lower()]
            print(f"### {m} {p}")
            print(yaml.safe_dump(resolve(op, 0, 2), sort_keys=False, width=160))
