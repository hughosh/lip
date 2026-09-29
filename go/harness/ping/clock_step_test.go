package ping

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"lip/harness/quote"
	"lip/harness/risk"
)

// A correction to civil time cannot make an hourly heartbeat fire early or
// leave the dead man silent for an extra hour. The same elapsed-time rule
// applies to a pending SEV2 suppression bucket.
func TestPairedClockKeepsHeartbeatAndBucketElapsedAcrossWallStep(t *testing.T) {
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			f := newFixture(t)
			srv := httptest.NewServer(newRecorder())
			defer srv.Close()
			dsrv := httptest.NewTLSServer(newRecorder())
			defer dsrv.Close()
			svc := f.service(senderTo(t, srv, "topic-clock"), deadmanTo(t, dsrv), time.Hour)
			ctx := context.Background()
			const wall0 int64 = 1_700_000_000_000
			const minute int64 = 60_000
			hb := heartbeatFor(quote.Running)

			f.anomaly("first", "CLOCK_TEST", risk.SEV2, "KXTEST-A", wall0)
			eff := svc.StepAt(ctx, wall0, 1, hb)
			if !eff.Heartbeat || len(pushesOf(eff, PushAnomaly)) != 1 {
				t.Fatalf("initial heartbeat and alert: %+v", eff)
			}
			f.settle()
			f.anomaly("suppressed", "CLOCK_TEST", risk.SEV2, "KXTEST-A", wall0+minute)
			f.settle()

			// Ten elapsed minutes, then a one-hour civil clock correction.
			wall := wall0 + 10*minute + step.Milliseconds()
			eff = svc.StepAt(ctx, wall, 1+10*minute, hb)
			if eff.Heartbeat || len(pushesOf(eff, PushAnomaly)) != 0 {
				t.Fatalf("wall step fired heartbeat or bucket early: %+v", eff)
			}
			if want := wall + 5*minute; eff.NextStepMs != want {
				t.Fatalf("bucket deadline = %d, want %d after 5 elapsed minutes", eff.NextStepMs, want)
			}

			wall += 5 * minute
			eff = svc.StepAt(ctx, wall, 1+15*minute, hb)
			if eff.Heartbeat || len(pushesOf(eff, PushAnomaly)) != 1 {
				t.Fatalf("bucket did not reopen at 15 elapsed minutes: %+v", eff)
			}
			f.settle()
			wall += 45 * minute
			eff = svc.StepAt(ctx, wall, 1+60*minute, hb)
			if !eff.Heartbeat || len(pushesOf(eff, PushHeartbeat)) != 1 || !eff.Deadman {
				t.Fatalf("heartbeat/dead man did not fire at 60 elapsed minutes: %+v", eff)
			}
		})
	}
}

// A failed attempt recovered from the durable queue starts with a wall stamp.
// Once this process has observed its remaining backoff, a later wall correction
// must not move that retry interval.
func TestPairedClockKeepsRecoveredRetryElapsedAcrossWallStep(t *testing.T) {
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			f := newFixture(t)
			rec := newRecorder()
			rec.setStatus(500)
			srv := httptest.NewServer(rec)
			defer srv.Close()
			dsrv := httptest.NewTLSServer(newRecorder())
			defer dsrv.Close()
			sender := senderTo(t, srv, "topic-recovered-clock")
			dead := deadmanTo(t, dsrv)
			ctx := context.Background()
			hb := heartbeatFor(quote.Running)
			const wall0 int64 = 1_700_000_000_000
			f.anomaly("retry", "CLOCK_RETRY", risk.SEV1, "KXTEST-A", wall0)
			first := f.service(sender, dead, time.Hour)
			if got := len(pushesOf(first.StepAt(ctx, wall0, 0, hb), PushAnomaly)); got != 1 {
				t.Fatalf("initial failed push count = %d", got)
			}
			f.settle()
			f.restart("runb", wall0+100)
			svc := f.service(sender, dead, time.Hour)
			if got := len(pushesOf(svc.StepAt(ctx, wall0+500, 500, hb), PushAnomaly)); got != 0 {
				t.Fatalf("recovered retry fired after 500ms: %d", got)
			}
			wall := wall0 + 500 + step.Milliseconds()
			if got := len(pushesOf(svc.StepAt(ctx, wall, 500, hb), PushAnomaly)); got != 0 {
				t.Fatalf("wall correction fired recovered retry early: %d", got)
			}
			if got := len(pushesOf(svc.StepAt(ctx, wall+500, 1000, hb), PushAnomaly)); got != 1 {
				t.Fatalf("recovered retry did not fire after 1000 elapsed ms: %d", got)
			}
		})
	}
}
