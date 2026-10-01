import yaml, collections
spec = yaml.safe_load(open('openapi.yaml'))

IMPLEMENTED = {
 ('POST','/portfolio/events/orders'),
 ('DELETE','/portfolio/events/orders/{order_id}'),
 ('GET','/portfolio/orders'),
 ('GET','/portfolio/orders/{order_id}'),
 ('GET','/portfolio/fills'),
 ('GET','/portfolio/positions'),
 ('GET','/portfolio/balance'),
 ('GET','/portfolio/subaccounts/balances'),
 ('GET','/markets/{ticker}'),
 ('GET','/markets/{ticker}/orderbook'),
 ('GET','/incentive_programs'),
}
# class: N needed for any generic trading client; U useful (many strategies / low-latency makers); S niche
CLS = {
 # orders
 ('DELETE','/portfolio/events/orders'):'N',
 ('POST','/portfolio/events/orders/batched'):'U',
 ('DELETE','/portfolio/events/orders/batched'):'U',
 ('POST','/portfolio/events/orders/{order_id}/amend'):'U',
 ('POST','/portfolio/events/orders/{order_id}/decrease'):'U',
 ('GET','/portfolio/orders/queue_positions'):'U',
 ('GET','/portfolio/orders/{order_id}/queue_position'):'U',
 # market data
 ('GET','/markets'):'N',
 ('GET','/markets/orderbooks'):'U',
 ('GET','/markets/trades'):'U',
 ('GET','/markets/candlesticks'):'S',
 ('GET','/series/{series_ticker}/markets/{ticker}/candlesticks'):'S',
 ('GET','/series'):'U',
 ('GET','/series/{series_ticker}'):'U',
 ('GET','/series/fee_changes'):'U',
 ('GET','/events'):'U',
 ('GET','/events/{event_ticker}'):'U',
 ('GET','/events/fee_changes'):'U',
 ('GET','/events/{event_ticker}/metadata'):'S',
 ('GET','/events/multivariate'):'S',
 ('GET','/series/{series_ticker}/events/{ticker}/candlesticks'):'S',
 ('GET','/series/{series_ticker}/events/{ticker}/forecast_percentile_history'):'S',
 # exchange
 ('GET','/exchange/status'):'N',
 ('GET','/exchange/schedule'):'U',
 ('GET','/exchange/user_data_timestamp'):'U',
 # account
 ('GET','/account/limits'):'U',
 ('GET','/account/endpoint_costs'):'U',
 ('GET','/account/api_usage_level/volume_progress'):'S',
 ('POST','/account/api_usage_level/upgrade'):'S',
 # portfolio
 ('GET','/portfolio/settlements'):'N',
 ('GET','/portfolio/summary/total_resting_order_value'):'U',
 ('POST','/portfolio/intra_exchange_instance_transfer'):'N',
 ('GET','/portfolio/target_balance_allocation'):'U',
 ('POST','/portfolio/target_balance_allocation'):'U',
}
# tag-level defaults for the rest
TAGCLS = {
 'order-groups':'U', 'communications':'S', 'api-keys':'S', 'fcm':'S', 'historical':'S',
 'live-data':'S', 'milestone':'S', 'structured-targets':'S', 'search':'S', 'multivariate':'S',
 'portfolio':'S',
}
rows=[]
for p,item in spec['paths'].items():
    for m,op in item.items():
        if m not in ('get','post','put','delete','patch'): continue
        key=(m.upper(),p)
        tag=(op.get('tags') or [''])[0]
        rows.append((tag,key,op.get('operationId',''),op.get('summary',''),op.get('deprecated',False)))
impl=[r for r in rows if r[1] in IMPLEMENTED]
unimpl=[r for r in rows if r[1] not in IMPLEMENTED]
assert len(impl)==11, len(impl)
print("total ops",len(rows),"implemented",len(impl),"unimplemented",len(unimpl))
cnt=collections.Counter()
by_tag=collections.defaultdict(list)
for r in unimpl:
    c=CLS.get(r[1]) or TAGCLS.get(r[0])
    assert c, r
    cnt[c]+=1
    by_tag[r[0]].append((c,r))
print("class counts (unimplemented):",dict(cnt))
print()
for tag in sorted(by_tag):
    lst=by_tag[tag]
    cs=collections.Counter(c for c,_ in lst)
    print(f"{tag}: {len(lst)} ops  {dict(cs)}")
import json
json.dump([[r[0],r[1][0],r[1][1],r[2],r[3],(CLS.get(r[1]) or TAGCLS.get(r[0])),r[4]] for r in unimpl], open('unimpl.json','w'))
