package main

import "testing"

func TestTurnoverConfigRequiresBoundedExplicitFundedCandidates(t *testing.T) {
	valid := `"turnover":true,"candidates":["KXTEST-A","KXTEST-B"],"capital_source":"selected_shard_balance",`
	body := func(fields string) string {
		return `{"ticker":"KXTEST-A","rung":"pilot","s":1,` + fields + goodTail(t) + `}`
	}
	c, err := loadConfig(writeConfig(t, body(valid)))
	if err != nil || !c.Turnover || len(c.Candidates) != 2 {
		t.Fatalf("explicit turnover rejected: %+v %v", c, err)
	}
	for _, fields := range []string{
		`"turnover":true,"candidates":["KXTEST-A"],`,
		`"turnover":true,"capital_source":"selected_shard_balance",`,
		`"turnover":true,"candidates":["KXTEST-B"],"capital_source":"selected_shard_balance",`,
		`"turnover":true,"candidates":["KXTEST-A","KXTEST-A"],"capital_source":"selected_shard_balance",`,
		`"turnover":true,"candidates":["KXTEST-A",""],"capital_source":"selected_shard_balance",`,
		`"candidates":["KXTEST-A"],"capital_source":"selected_shard_balance",`,
	} {
		if _, err := loadConfig(writeConfig(t, body(fields))); err == nil {
			t.Fatalf("accepted invalid turnover config: %s", fields)
		}
	}
}

// confidence: high
