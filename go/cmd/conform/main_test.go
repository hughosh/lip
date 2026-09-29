package main

import "testing"

// The live-shape survey must fetch the same program set as the harness. Its
// deliberately wider survey remains labelled latent, not a live refusal.
func TestProgramProbesMatchProductionFilters(t *testing.T) {
	var production, wider bool
	for _, p := range probes() {
		switch p.name {
		case "programs":
			production = true
			if p.latent || p.filters.Get("status") != "active" ||
				p.filters.Get("type") != "liquidity" {
				t.Fatalf("production program probe has wrong scope: %+v", p)
			}
		case "programs.unfiltered":
			wider = true
			if !p.latent || len(p.filters) != 0 {
				t.Fatalf("wide program probe must stay unfiltered and latent: %+v", p)
			}
		}
	}
	if !production || !wider {
		t.Fatalf("program probes missing: production=%v wider=%v", production, wider)
	}
}
