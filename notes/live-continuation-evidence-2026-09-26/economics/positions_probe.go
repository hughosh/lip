// Temporary, structurally GET-only account evidence probe. Run from go/ with
// go run ../notes/live-continuation-evidence-2026-09-26/economics/read_probe.go.
package main

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "time"

 "lip/feed"
 "lip/harness/rest"
)

type getOnly struct{ next rest.Doer }
func (g getOnly) Do(ctx context.Context, r rest.Request) (rest.Response,error) {
 if r.Method!="GET" || len(r.Body)!=0 || r.Path!="/portfolio/positions" {
  return rest.Response{},errors.New("GET-only account probe refused request")
 }
 return g.next.Do(ctx,r)
}
func selectFields(rows []json.RawMessage, fields []string) ([]map[string]any,error) {
 out:=make([]map[string]any,0,len(rows))
 for _,raw:=range rows {
  var full map[string]json.RawMessage
  if err:=json.Unmarshal(raw,&full);err!=nil { return nil,errors.New("row decode failed") }
  row:=map[string]any{}
  for _,k:=range fields {
   if b,ok:=full[k];ok {
    var v any
    if err:=json.Unmarshal(b,&v);err!=nil { return nil,errors.New("field decode failed") }
    row[k]=v
   }
  }
  out=append(out,row)
 }
 return out,nil
}
func main() {
 signer,err:=feed.NewSigner();if err!=nil { fmt.Fprintln(os.Stderr,"credentials unavailable");os.Exit(1) }
 ctx,cancel:=context.WithTimeout(context.Background(),45*time.Second);defer cancel()
 client:=rest.NewClient(getOnly{next:rest.NewHTTPDoer(signer,15*time.Second)})
 ep:=rest.Endpoint{Path:"/portfolio/positions",CursorField:"cursor",ItemKeys:[]string{"market_positions","event_positions"},MinLimit:1,MaxLimit:1000,PageLimit:1000,MaxPages:1000}
 start:=time.Now().UTC().Format(time.RFC3339Nano)
 walk:=client.Walk(ctx,ep,nil)
 fields:=[]string{"ticker","event_ticker","exchange_index","position_fp","total_traded_dollars","market_exposure_dollars","realized_pnl_dollars","fees_paid_dollars","total_cost_dollars","total_cost_shares_fp","event_exposure_dollars","last_updated_ts"}
 markets,me:=selectFields(walk.Records("market_positions"),fields)
 events,ee:=selectFields(walk.Records("event_positions"),fields)
 if me!=nil||ee!=nil { fmt.Fprintln(os.Stderr,"decode failed");os.Exit(1) }
 result:=map[string]any{"started_at":start,"ended_at":time.Now().UTC().Format(time.RFC3339Nano),"subaccount":0,"exchange_index":"all; parameter omitted","status":walk.Outcome.String(),"pages":walk.Pages,"market_positions":markets,"event_positions":events}
 b,err:=json.MarshalIndent(result,"","  ");if err!=nil { os.Exit(1) }
 if err:=os.WriteFile("../notes/live-continuation-evidence-2026-09-26/economics/positions-pnl.json",append(b,'\n'),0600);err!=nil { os.Exit(1) }
 fmt.Fprintln(os.Stderr,"GET-only positions evidence written")
}
