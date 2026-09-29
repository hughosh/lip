package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"lip/harness/hstore"
	"lip/harness/ping"
	"lip/harness/qual"
	"lip/harness/quote"
)

// alertStepper is the whole runtime surface of §13.  It is required rather
// than defaulted: forgetting the production service must make composition fail,
// not quietly create a process which trades without telling anyone.
type alertStepper interface {
	Step(context.Context, int64, ping.Heartbeat) ping.Effects
}

// pairedAlertStepper is implemented by the production service. Test steppers
// that only implement Step keep the original deterministic seam.
type pairedAlertStepper interface {
	StepAt(context.Context, int64, int64, ping.Heartbeat) ping.Effects
}

// alertFactory delays only the store-dependent half of construction.  The
// production factory has already loaded and validated both destinations before
// the first exchange request; NewService is called later, after the store's
// reader and single writer exist.
type alertFactory func(*hstore.Reader, *hstore.Store, time.Duration) (alertStepper, error)

func productionAlertFactory(envPath string) (alertFactory, error) {
	topic, err := ping.LoadNTFYTopic(envPath)
	if err != nil {
		return nil, classifyAlertDestination(fmt.Errorf("loading the alert topic: %w", err))
	}
	sender, err := ping.NewNTFYSender(topic)
	if err != nil {
		return nil, classifyAlertDestination(fmt.Errorf("constructing the alert sender: %w", err))
	}
	dead, err := ping.LoadHTTPSDeadman(envPath)
	if err != nil {
		return nil, classifyAlertDestination(fmt.Errorf("loading the dead-man endpoint: %w", err))
	}
	return func(reader *hstore.Reader, store *hstore.Store,
		interval time.Duration) (alertStepper, error) {
		return ping.NewService(reader, store, sender, dead, interval)
	}, nil
}

// A read failure may clear on the next supervised launch. A readable file
// with an absent, duplicate, or malformed destination needs an operator edit.
func classifyAlertDestination(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return err
	}
	return &refusal{err: err}
}

func (r *rig) startAlerts(ctx context.Context) error {
	if r.alerts == nil {
		return errors.New("the rig has no alert service; unattended operation " +
			"requires a concrete §13 delivery policy")
	}
	r.alertMu.Lock()
	defer r.alertMu.Unlock()
	if r.alertDone != nil && !r.alertStopped {
		return errors.New("the alert loop was started more than once")
	}
	previous := r.alertDone
	r.alertParent = ctx
	r.alertStopped = false
	alertCtx, cancel := context.WithCancel(ctx)
	r.alertCancel = cancel
	r.alertDone = make(chan struct{})
	done := r.alertDone
	go func() {
		if previous != nil {
			<-previous
		}
		if alertCtx.Err() != nil {
			close(done)
			return
		}
		r.runAlerts(alertCtx, done)
	}()
	return nil
}

func (r *rig) runAlerts(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	next := r.stepAlerts(ctx)
	for {
		timer := time.NewTimer(alertDelay(r.ex.NowMs(), next))
		select {
		case <-ctx.Done():
			stopAlertTimer(timer)
			return
		case <-timer.C:
		case <-r.alertWake:
			stopAlertTimer(timer)
		case ack := <-r.alertFlush:
			stopAlertTimer(timer)
			next = r.stepAlerts(ctx)
			close(ack)
			continue
		}
		next = r.stepAlerts(ctx)
	}
}

// alertDelay is capped so store health is still observed even if a defective
// stepper publishes an implausibly distant deadline.  Production's service
// normally publishes its own one-second health poll or an earlier retry.
func alertDelay(nowMs, nextMs int64) time.Duration {
	const ceiling = time.Second
	if nextMs == 0 {
		return ceiling
	}
	if nextMs < nowMs {
		return time.Millisecond
	}
	d := time.Duration(nextMs-nowMs) * time.Millisecond
	if d > ceiling {
		return ceiling
	}
	return d
}

// Stop may report false while the runtime is in the act of publishing the
// timer value.  A blocking receive in that window can wait forever because the
// selected wake or cancellation has already won.  Drain only if the value is
// already available.
func stopAlertTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (r *rig) stepAlerts(ctx context.Context) int64 {
	wallMs, monoMs := r.ex.NowMs(), r.ex.Mono().Milliseconds()
	hb := r.heartbeat()
	var eff ping.Effects
	if paired, ok := r.alerts.(pairedAlertStepper); ok {
		eff = paired.StepAt(ctx, wallMs, monoMs, hb)
	} else {
		eff = r.alerts.Step(ctx, wallMs, hb)
	}
	r.observeAlertEffects(eff)
	return eff.NextStepMs
}

func (r *rig) wakeAlerts() {
	select {
	case r.alertWake <- struct{}{}:
	default:
	}
}

// flushAlerts asks the alert goroutine to perform the final Step.  Step is
// stateful (retry ladders and suppression buckets), so shutdown sends a command
// to its sole owner instead of calling it concurrently.
func (r *rig) flushAlerts(ctx context.Context) error {
	r.alertMu.Lock()
	done := r.alertDone
	stopped := r.alertStopped
	r.alertMu.Unlock()
	if done == nil || stopped {
		return errors.New("the final alert step requires a running alert loop")
	}
	ack := make(chan struct{})
	select {
	case r.alertFlush <- ack:
	case <-done:
		return errors.New("the alert loop stopped before its final step")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-done:
		return errors.New("the alert loop stopped during its final step")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *rig) stopAlerts(ctx context.Context) error {
	r.alertMu.Lock()
	done := r.alertDone
	cancel := r.alertCancel
	if done != nil && !r.alertStopped {
		r.alertStopped = true
		cancel()
	}
	r.alertMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the alert loop to stop: %w", ctx.Err())
	}
}

// resumeAlerts preserves the service's retry/suppression state on a refused
// close. A canceled step must join before a new sole owner can use that state.
func (r *rig) resumeAlerts() {
	r.alertMu.Lock()
	parent, stopped := r.alertParent, r.alertStopped
	r.alertMu.Unlock()
	if parent == nil || !stopped {
		return
	}
	if parent.Err() == nil {
		if err := r.startAlerts(parent); err != nil {
			fmt.Fprintf(os.Stderr, "harness: restoring alert delivery after refused stop: %v\n", err)
		}
	}
}

func (r *rig) heartbeat() ping.Heartbeat {
	hb := ping.Heartbeat{
		Global:  quote.Starting,
		Capital: ping.KnownMoney(r.cfg.Params.CapitalMax),
		Uptime:  ping.KnownDuration(r.ex.Mono()),
		// Integrated remains deliberately unknown.  The command currently has
		// no authoritative integrated-presence accumulator; rendering zero here
		// would claim a measurement that does not exist.
	}
	if r.snap != nil {
		if snap := r.snap.Load(); snap != nil {
			hb.Global = snap.Global
			hb.Markets = make([]ping.MarketLine, 0, len(snap.Markets))
			for _, market := range snap.Markets {
				hb.Markets = append(hb.Markets, ping.MarketLine{
					Ticker: market.Ticker,
					State:  market.State,
					Q:      ping.KnownQty(market.Q),
				})
			}
		}
	}
	if last := r.last.Load(); last != nil {
		hb.SourceStale = ping.KnownBool(last.Stale)
	}
	if r.store != nil {
		if n, err := r.store.Reader().UndeliveredCount(); err == nil {
			hb.Undelivered = ping.KnownInt(int64(n))
		}
	}
	return hb
}

func (r *rig) observeAlertEffects(eff ping.Effects) {
	if eff.Err != nil {
		fmt.Fprintln(os.Stderr, "harness: alert queue could not be read; retry scheduled")
	}
	for _, push := range eff.Pushes {
		if push.Err != nil {
			fmt.Fprintf(os.Stderr, "harness: %s delivery failed; durable rows remain pending and will retry\n", push.Kind)
			continue
		}
		if push.Kind == ping.PushHeartbeat {
			r.recordAlertEvent("sent")
		}
	}
	if eff.DeadmanErr != nil {
		fmt.Fprintln(os.Stderr, "harness: dead-man check-in failed; retry scheduled")
	}
	if eff.Deadman {
		r.recordAlertEvent("deadman_checkin_sent")
	}
}

func (r *rig) recordAlertEvent(name string) {
	if r.qual == nil {
		return
	}
	if err := r.qual.RecordEvent(qual.EventHeartbeat, name, time.Now().UTC()); err != nil {
		r.failQualification(fmt.Errorf("recording %s qualification event: %w", name, err))
	}
}

// confidence: high
