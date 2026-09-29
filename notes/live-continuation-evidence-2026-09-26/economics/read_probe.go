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
 if r.Method!="GET" || len(r.Body)!=0 || (r.Path!="/portfolio/settlements" && r.Path!="/portfolio/fills") {
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
 ep:=rest.Endpoint{Path:"/portfolio/settlements",CursorField:"cursor",ItemKeys:[]string{"settlements"},MinLimit:1,MaxLimit:1000,PageLimit:1000,MaxPages:1000}
 start:=time.Now().UTC().Format(time.RFC3339Nano)
 sw:=client.Walk(ctx,ep,nil)
 fw:=client.Walk(ctx,rest.EpFills,nil)
 sr,se:=selectFields(sw.Records("settlements"),[]string{"ticker","exchange_index","event_ticker","market_result","yes_count_fp","yes_total_cost_dollars","no_count_fp","no_total_cost_dollars","revenue","settled_time","fee_cost","value"})
 fr,fe:=selectFields(fw.Records("fills"),[]string{"ticker","exchange_index","outcome_side","book_side","side","action","count_fp","yes_price_dollars","no_price_dollars","is_taker","fee_cost","created_time","subaccount_number","ts"})
 if se!=nil||fe!=nil { fmt.Fprintln(os.Stderr,"row decode failed");os.Exit(1) }
 result:=map[string]any{"started_at":start,"ended_at":time.Now().UTC().Format(time.RFC3339Nano),"settlements_status":sw.Outcome.String(),"settlements_pages":sw.Pages,"settlements_count":len(sr),"settlements":sr,"fills_status":fw.Outcome.String(),"fills_pages":fw.Pages,"fills_count":len(fr),"fills":fr}
 b,err:=json.MarshalIndent(result,"","  ");if err!=nil { fmt.Fprintln(os.Stderr,"report encode failed");os.Exit(1) }
 if err:=os.WriteFile("../notes/live-continuation-evidence-2026-09-26/economics/account-read.json",append(b,'\n'),0600);err!=nil { fmt.Fprintln(os.Stderr,"report write failed");os.Exit(1) }
 fmt.Fprintf(os.Stderr,"GET-only evidence written: settlements=%s/%d pages/%d rows fills=%s/%d pages/%d rows\n",sw.Outcome.String(),sw.Pages,len(sr),fw.Outcome.String(),fw.Pages,len(fr))
}
