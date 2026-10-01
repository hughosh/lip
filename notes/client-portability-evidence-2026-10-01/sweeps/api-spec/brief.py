import yaml, sys
spec = yaml.safe_load(open('openapi.yaml'))
def getp(prm):
    if '$ref' in prm:
        name = prm['$ref'].split('/')[-1]
        prm = spec['components']['parameters'][name]
    return prm
def resolve_schema(name):
    return spec['components']['schemas'][name]
def brief(opkey, show_resp_props=True):
    m,p = opkey.split(' ',1)
    op = spec['paths'][p][m.lower()]
    print("=====", opkey, op.get('operationId'), "security=", bool(op.get('security')))
    d = op.get('description','').strip().replace('\n',' ')
    if d: print("desc:", d[:500])
    for prm in op.get('parameters',[]):
        prm = getp(prm)
        sch = prm.get('schema',{})
        if '$ref' in sch: sch = {'$ref': sch['$ref'].split('/')[-1]}
        keep = {k:v for k,v in sch.items() if k in ('type','enum','$ref','default','minimum','maximum','format')}
        print(f"  param {prm['name']} in={prm['in']} req={prm.get('required',False)} {keep} -- {str(prm.get('description',''))[:150]!r}")
    for code, r in op['responses'].items():
        if code != '200' and code != '201': continue
        c = r.get('content',{}).get('application/json',{}).get('schema',{})
        ref = c.get('$ref','').split('/')[-1]
        print("  resp", code, ref)
        if show_resp_props and ref:
            s = resolve_schema(ref)
            props = s.get('properties', {})
            print("    required:", s.get('required'))
            print("    props:", list(props.keys()))
if __name__=='__main__':
    for a in sys.argv[1:]:
        brief(a)
