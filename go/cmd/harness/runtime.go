package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"lip/core"
	"lip/feed"
	"lip/harness/hstore"
	"lip/harness/lifecycle"
	"lip/harness/netx"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// This file is the composition root, and until it existed no code in this
// repository had ever known these packages at the same time.
//
// Every component below is individually complete, individually unit-tested and
// individually guarded by a mutation with a declared catcher. None of them had
// a production caller. `quote.Decide` had none outside its own tests;
// `wsx.Poller` was finished and nothing consumed its channel; the whole of
// `harness/lifecycle` was reachable only from `lifecycle_test`. So the risk
// here is not that a part is wrong -- the parts are the most heavily verified
// code in the tree -- it is that two parts are joined in the wrong ORDER, with
// the wrong lifetime, or on the wrong goroutine.
//
// The construction order below is therefore load-bearing in five places, and
// each one is annotated where it happens:
//
//  1. the instance lock is taken before ANYTHING else (H-DEP-5), and in
//     particular before the first REST request;
//  2. the store is OPENED and never created here (H-ORD-9): a fresh ledger
//     recognises no order id and makes every fill on the account foreign;
//  3. the writer goroutine starts before the first submission, because a
//     submission to a store with no writer is a record that is accepted and
//     never written;
//  4. the durable latch is read, through `NewGlobalController`, before the
//     first portfolio request (H-HALT-4);
//  5. the run row COMMITS before anything that references it exists.

// restTimeout bounds one REST request.
//
// It is not a §16 knob and must not become one: §16 is the table the `run` row
// records verbatim, and a deployment-shaped number in it would make two
// deployments of the same configuration differ in their recorded configuration
// (the argument `hstore.StoreConfig` makes about paths).
//
// Ten seconds is chosen against `truth_max_age_s`, not against comfort. One
// poll cycle is three sequential walks, so a completely wedged endpoint costs
// at most 30 s of a 60 s freshness budget: truth ages out and H-FAIL-4 stops
// dispatch, which is the correct end state. A timeout at or above
// `truth_max_age_s` would instead let a single hung request hold the poll
// goroutine past the point where its own answer had expired.
const restTimeout = 10 * time.Second

// dispatchWorkers is H-TOP-4's K for the pilot profile: one.
//
// D3 and H-ORD-6 and "ONE REST writer" are the same requirement stated three
// times, and one goroutine is how it is enforced rather than remembered.
//
// `quote.NewCapacity` raises this to two internally, because H-QUE-3 requires a
// P1 reserve that is never the whole pool and a pool of one cannot carve one.
// The consequence is worth stating plainly: with a single dispatcher goroutine
// at most one write is ever in flight, so the WORKER dimension of `Capacity` is
// never the binding constraint for the pilot. The §16 token bucket is, and the
// dispatcher owns its refill.
const dispatchWorkers = 1

// anomalyBuffer is the depth of the hand-off between every producer of an
// anomaly and the single goroutine that submits them.
//
// The channel exists because of I2 and H-STORE-2. The monitor must be unable to
// block on a disk -- "a monitor that could block on a disk is a monitor that a
// disk can stop" -- and the poll loop must not either, because the anomalies it
// most needs to raise are the ones about the store. A buffered send with a
// non-blocking fallback is the only shape that satisfies both.
//
// 256 is roughly a minute of the worst sustained rate this system produces: one
// SEV2 per market per poll at 5 s, plus the monitor's 1 Hz. A burst past it
// drops, and the drop is COUNTED and reported rather than silent -- see
// `anomalySink`.
const anomalyBuffer = 256

// exchange is every collaborator that reaches outside this process.
//
// It is an explicit argument rather than something `newRig` constructs, and
// there is no nil-means-production default in it. Two reasons, and the second
// is the one that matters:
//
//   - the seam tests are integration-shaped by necessity (every bug in this
//     bead is at a join, so a unit test of an already-proven part finds
//     nothing), and they drive a fake exchange through this struct;
//   - a field that falls back to a live collaborator when it is nil is a field
//     that silently reaches the real account when a caller forgets it.
//     `lifecycle` states the same rule as a test --
//     `TestNoProductionDefaultStandsInForADeferredCollaborator` -- and the
//     account this one would reach holds real money.
type exchange struct {
	// Doer is the signed HTTP transport `rest.Client` writes through.
	Doer rest.Doer
	// Dialer and Signer are the websocket half. Signer is `wsx.Signer` (the
	// handshake headers) and not `*feed.Signer`, because that is all the
	// supervisor may do with a credential.
	Dialer wsx.Dialer
	Signer wsx.Signer
	// Clock is `wsx`'s injected clock: monotonic readings and timers.
	Clock wsx.Clock

	// Target is the market's LIP Target Size, from the incentive-programs
	// endpoint. It is `core.Book.Qualifies()`'s threshold, and a book built with
	// a zero target reports EVERY interval as gated -- which reads as "nobody
	// could have scored here" for a market that is in fact paying.
	Target float64

	// NowMs stamps RECORDS and NOTHING else. Wall milliseconds.
	NowMs func() int64
	// Mono measures ELAPSED time and is never written to a record. Separate
	// from NowMs because F21 (a clock step) and F7 (host sleep) both turn on
	// the two being distinguishable.
	Mono func() time.Duration
}

func (e exchange) validate() error {
	var missing []string
	if e.Doer == nil {
		missing = append(missing, "Doer")
	}
	if e.Dialer == nil {
		missing = append(missing, "Dialer")
	}
	if e.Signer == nil {
		missing = append(missing, "Signer")
	}
	if e.Clock == nil {
		missing = append(missing, "Clock")
	}
	if e.NowMs == nil {
		missing = append(missing, "NowMs")
	}
	if e.Mono == nil {
		missing = append(missing, "Mono")
	}
	if len(missing) > 0 {
		return fmt.Errorf("exchange is missing %v; there is deliberately no "+
			"production default for any of them, because a field that reaches "+
			"the live account when a caller forgets it is a field that reaches "+
			"it during a test", missing)
	}
	if e.Target <= 0 {
		return fmt.Errorf("exchange Target Size is %v; a book built with a "+
			"non-positive target has Qualifies() == 0 forever, so every "+
			"interval reads as gated and the market appears to pay nobody",
			e.Target)
	}
	return nil
}

// ---------------------------------------------------------------------------
// F6 -- the network layer
// ---------------------------------------------------------------------------

// f6Net is the process's ONE resolver cache and the TWO transports over it.
//
// One cache, because the point of §F6's floor TTL is that a host resolved by
// any part of the process is a host every other part can still reach when the
// system resolver wedges. Two transports over it, because H-FAIL-2 requires
// REST and the websocket to be separately poolable: REST is how a cancel still
// reaches the exchange when the feed is gone, and a connection-pool fault
// shared between them takes out the escape route along with the feed.
type f6Net struct {
	dialer *netx.CachedDialer
	rest   *http.Transport
	ws     *http.Transport
}

// newF6Net composes F6 over injected system collaborators.
//
// The resolver, the dial and the clock are arguments rather than defaults for
// the same reason `netx.NewCachedDialer` insists on them: the fallback is
// reachable only from a resolver that can be made to fail, and a composition
// root that hard-coded the real ones would leave the production wiring -- which
// is what the two BYPASS mutations attack -- with no test that traverses it.
func newF6Net(anom *anomalySink, r netx.Resolver, d netx.DialFunc,
	now func() time.Time) (*f6Net, error) {

	if anom == nil {
		return nil, fmt.Errorf("the F6 network layer needs the process anomaly " +
			"sink; a nil one would make the resolver fall back silently, and a " +
			"silent fallback is the 2.5-hour wedge going unnoticed for exactly " +
			"as long as it did before this package existed")
	}
	cd, err := netx.NewCachedDialer(r, d, now, dnsFallbackReporter(anom))
	if err != nil {
		return nil, err
	}
	return &f6Net{dialer: cd, rest: f6Transport(cd), ws: f6Transport(cd)}, nil
}

// f6Transport is `net/http`'s default transport with ONLY its dial replaced.
//
// The fields are restated rather than cloned from `http.DefaultTransport` so
// that what production runs on is written down: this is the same transport the
// harness has always had, plus the cache. Nothing here touches TLS, so
// `crypto/tls` still derives `ServerName` from the URL and still verifies the
// certificate against the real hostname -- which is the whole reason F6 is a
// dialer (see `netx.CachedDialer.DialContext`).
func f6Transport(cd *netx.CachedDialer) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           cd.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// dnsFallbackReporter is §F6's queued SEV2, on the process sink.
//
// It is account-scoped -- `Ticker` is deliberately empty -- because a wedged
// resolver is a property of the host, not of a market, and attributing it to
// the market that happened to be quoting would make it look market-specific in
// the anomaly journal.
//
// `anomalySink.raise` is a non-blocking send into a buffered channel, which is
// what makes this legal to call from inside the dial path at all:
// `netx.Reporter` MUST NOT block, and a reporter that took a lock or wrote a
// file would turn F6's survival path into a second stall.
//
// `M-7ZT-NOALERT` drops the report on the floor, which leaves the harness
// silently running on a cached address -- the fallback working perfectly and
// nobody ever learning that it was needed.
func dnsFallbackReporter(anom *anomalySink) netx.Reporter {
	return func(f netx.Fallback) {
		anom.raise(dnsFallbackAnomaly(f))
	}
}

// dnsFallbackAnomaly is the §13.1 record one fallback produces.
//
// It is split from the reporter so the TEXT is a pure function of the fallback
// and can be asserted without a sink, and so that the one line that hands it to
// the sink has nothing else in it -- which is the line `M-7ZT-NOALERT` removes.
func dnsFallbackAnomaly(f netx.Fallback) risk.Anomaly {
	outcome := "which connected"
	if !f.Connected {
		outcome = "WHICH DID NOT CONNECT EITHER"
	}
	return risk.Anomaly{
		Class: "DNS_FALLBACK",
		Sev:   risk.SEV2,
		Text: fmt.Sprintf("resolving %s failed (%v); dialled the "+
			"last-known-good address %s instead, %s. If `nslookup` answers "+
			"normally this is the system resolver wedging (F6), not the "+
			"network going away", f.Host, f.LookupErr, f.Address, outcome),
	}
}

// productionExchange builds the collaborators that reach the real account.
//
// The Target Size is fetched here rather than configured, and the ticker is
// REQUIRED to be in the active set. A market outside the incentive programme
// earns nothing however well we quote it, and a stale target in a config file
// is the kind of number nobody re-reads: quoting to a threshold the exchange
// stopped using produces a harness that believes it is qualifying and is not.
//
// It takes the process anomaly sink rather than making one, because the DNS
// fallback raised from inside the dial path and every anomaly the rig raises
// have to arrive in the same queue and the same durable journal. Two sinks
// would mean the fallback was recorded somewhere nothing drains.
func productionExchange(ctx context.Context, c config, anom *anomalySink) (exchange, error) {
	signer, err := feed.NewSignerFrom(c.Paths.Key, c.Paths.Env)
	if err != nil {
		return exchange{}, fmt.Errorf("credentials: %w", err)
	}

	// The ONLY place the real resolver, the real dial and the real clock are
	// named. `restTimeout` bounds the connect for the same reason it bounds the
	// request: a dial that hangs holds the poll goroutine past the point where
	// its own answer would have expired.
	nt, err := newF6Net(anom, netx.SystemResolver(),
		(&net.Dialer{Timeout: restTimeout}).DialContext, time.Now)
	if err != nil {
		return exchange{}, err
	}
	return exchangeOver(ctx, c, signer, nt)
}

// exchangeOver builds the exchange over an ALREADY-COMPOSED network layer.
//
// This is the production wiring, and it is a separate function only so that a
// test can reach it with a scripted resolver. Both BYPASS mutations live in the
// three lines below: `M-7ZT-RESTBYPASS` builds the Doer on the default
// transport and `M-7ZT-WSBYPASS` builds the Dialer on it, and either one leaves
// F6 implemented, tested, and on no path the harness actually uses -- which is
// the shape H-CAP-8 has already had once in this tree.
func exchangeOver(ctx context.Context, c config, signer *feed.Signer,
	nt *f6Net) (exchange, error) {

	doer := rest.NewHTTPDoerWithTransport(signer, restTimeout, nt.rest)

	// The active set is read through the cached transport, as a COMPLETE walk.
	// `feed.Universe` did this before and could do neither: it builds its own
	// transport over a bare dialer, and it reads one page of 200 and never
	// sends a cursor. It is hash-pinned, so the walk is rebuilt in `rest`
	// rather than repaired where it was.
	programs := rest.NewClient(doer).Programs(ctx)
	if !programs.Replaces() {
		return exchange{}, fmt.Errorf("reading the active LIP universe: the "+
			"walk %s after %d pages: %v", programs.Outcome, programs.Pages,
			programs.Err)
	}
	target, listed := programs.ByTarget[c.Ticker]
	if !listed {
		return exchange{}, fmt.Errorf("%s is not in the active LIP universe "+
			"(%d markets); quoting a market outside the incentive programme "+
			"earns nothing however well it is quoted, and there is no Target "+
			"Size for its qualifying walk", c.Ticker, len(programs.ByTarget))
	}

	start := time.Now()
	return exchange{
		Doer:   doer,
		Dialer: wsx.NewLiveDialerWithTransport(nt.ws),
		Signer: signer,
		Clock:  wsx.NewSystemClock(),
		Target: target,
		NowMs:  func() int64 { return time.Now().UnixMilli() },
		Mono:   func() time.Duration { return time.Since(start) },
	}, nil
}

// anomalySink is the non-blocking hand-off every goroutine raises through.
//
// A full buffer DROPS, and the drop is counted. That is the honest trade: the
// alternative is a monitor that stops sampling because a disk is slow, which is
// probebot.py's observable reached by a new route. The count is not swallowed
// -- `drain` synthesises a SEV1 naming it -- so a burst that overflowed is
// visible in the same journal as the anomalies that caused it.
type anomalySink struct {
	ch      chan risk.Anomaly
	dropped atomic.Uint64
}

func newAnomalySink() *anomalySink {
	return &anomalySink{ch: make(chan risk.Anomaly, anomalyBuffer)}
}

// raise never blocks and never fails. It is called from the monitor goroutine,
// the poll goroutine and the owner goroutine.
func (s *anomalySink) raise(a risk.Anomaly) {
	if a.Class == "" || a.Text == "" {
		// An anomaly the store would reject at submission. Raising it would
		// consume a buffer slot and produce nothing; counting it as a drop is
		// what makes the discrepancy visible.
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- a:
	default:
		s.dropped.Add(1)
	}
}

func (s *anomalySink) raiseAll(as []risk.Anomaly) {
	for _, a := range as {
		s.raise(a)
	}
}

// takeDropped resets and returns the drop count, for the submitter to report.
func (s *anomalySink) takeDropped() uint64 { return s.dropped.Swap(0) }

// noopSink satisfies `core.Sink` and records nothing.
//
// `core.Rig` is the differentially-tested book decoder and it is the only thing
// that produces the `*core.Levels` `quote.ExternalBest` reads, so the harness
// drives it. What the harness does NOT want is the measurement rig's output:
// fills, pending mids and reference rows are `rig.db`'s evidence, written by a
// collector that has been running since July, and H-ORD-7 makes that database
// read-only to this process.
//
// Discarding them here rather than not subscribing is deliberate. The trade
// channel is subscribed UNFILTERED by `wsx` on purpose ("one `trade`
// subscription yields the whole exchange tape"), and narrowing it would be a
// change to a hash-pinned decision made for the collector's benefit.
type noopSink struct{}

func (noopSink) Fill(core.FillRow)                  {}
func (noopSink) PendingMid(_, _, _ string, _ int64) {}
func (noopSink) Reference(core.ReferenceRow)        {}

// rig is every collaborator of one harness process, constructed once.
//
// It is never copied: it carries an `atomic.Pointer` through a pointer field
// and a `*hstore.Store` whose mutex protects a FIFO, and a copy of either is a
// second view of state that is supposed to have exactly one.
type rig struct {
	cfg config

	// --- lifecycle ---------------------------------------------------------

	lock  *lifecycle.InstanceLock
	latch *lifecycle.FileLatch
	ctrl  *lifecycle.GlobalController
	// boot is what reading the latch at construction produced. Held because
	// `BlockAdding` and `RetryLatch` are answers the run loop must honour on its
	// first tick, not diagnostics.
	boot   lifecycle.BootstrapEffects
	sigs   *lifecycle.SignalController
	drain  *lifecycle.DrainTracker
	guard  *lifecycle.ForeignGuard
	policy lifecycle.AdoptionPolicy
	start  *lifecycle.Startup

	// --- the store ---------------------------------------------------------

	store *hstore.Store
	// storeCancel stops the writer goroutine. It is handed to
	// `hstore.Shutdown`, which is the ONLY thing entitled to call it: the store
	// refuses to close over a non-empty FIFO, and cancelling the writer first
	// fails every record it was still holding.
	storeCancel context.CancelFunc
	storeDone   chan struct{}
	// run is §15's licence. Every record that references `run(run_id)` requires
	// it, and it exists only because the row COMMITTED.
	run   hstore.RunHandle
	runID string

	// deferred is any store result that arrived while construction was waiting
	// for the run handle. It is carried rather than dropped: a terminal result
	// is a record that is GONE, and `hstore.Rejections` is how the operator
	// hears about it.
	deferred []hstore.Result

	// --- the exchange ------------------------------------------------------

	ex   exchange
	api  *rest.Client
	sup  *wsx.Supervisor
	poll *wsx.Poller
	gate *wsx.Gate

	// --- the model ---------------------------------------------------------

	// book is the decoder. It is single-goroutine by construction (`core` holds
	// no mutex, exactly as `harness/quote` does not), so ONLY the owner
	// goroutine may call `Handle`, `Book` or `ResetOnReconnect`.
	book *core.Rig
	// pf is a placeholder until startup completes. §7.5's adoption produces the
	// real one, seeded from the exchange with every kept order installed, and
	// `run.go` installs it. A harness that quoted from this one would be
	// quoting from an account it had not read.
	pf *risk.Portfolio

	queue *quote.Queue
	cap   quote.Capacity

	// --- the monitor -------------------------------------------------------

	// snap is the ONLY channel between the owner and the monitor (I2).
	snap *atomic.Pointer[risk.Snapshot]
	mon  *monitor
	// last is the most recent monitor tick, for the heartbeat. §15's cut is
	// five tables and `snap` is not one of them, so samples are held in memory
	// for §13.3 rather than written.
	last atomic.Pointer[risk.StepResult]

	anom *anomalySink

	// cleanup unwinds a partially-built rig in reverse order. Construction here
	// acquires a lock, a database, a journal, a reader and a goroutine before it
	// can fail, and a constructor that returns an error having leaked any of
	// them leaves the next start refused by its own predecessor.
	cleanup []func()
}

// newRig composes one harness process. It starts no loops.
//
// `resume` is §10.4's operator action. A latched harness does not restart
// itself into RUNNING -- H-HALT-4's whole content is that the latch survives the
// process -- so a set latch without `resume` is refused HERE, at the point where
// the latch has just been read and nothing has been sent to the exchange.
// The rig under construction is a LOCAL, and the unwind closure closes over the
// local rather than over a named result. That is not a style preference; the
// obvious shape is wrong in a way that costs the entire refusal surface.
//
// With `(r *rig, err error)` as NAMED results and the unwind reading `r`, every
// refusal written `return nil, err` assigns nil to `r` BEFORE the deferred
// closure runs -- so the closure calls `unwind` on a nil `*rig` and the process
// dies of a nil dereference instead of returning the refusal. Every clause of
// the narrowed refusal goes that way: the instance lock (H-DEP-5), a set halt
// latch without `-resume` (H-HALT-4), a missing store, a run row that would not
// commit, and every collaborator constructor after them. `main.go` expects an
// error it can classify and exit `exitRefused` with; it would get a SIGSEGV,
// and `launchd KeepAlive` restarts a crash forever.
//
// Keeping the rig in a local the closure owns makes that unexpressible: there is
// no assignment any `return` can make that the unwind path can see.
func newRig(ctx context.Context, c config, resume bool, ex exchange,
	anom *anomalySink) (*rig, error) {

	if err := ex.validate(); err != nil {
		return nil, err
	}
	// The sink is the CALLER's, and there is no default for it here for the
	// same reason there is none for any field of `exchange`. It is created
	// before `productionExchange` so the DNS fallback -- which is raised from
	// inside the dial path, before this function has run and possibly before it
	// ever will -- lands in the queue this rig goes on to drain. A sink made
	// here would be a second one, and the fallback that preceded it would be
	// recorded somewhere nothing reads.
	if anom == nil {
		return nil, fmt.Errorf("the rig needs the process anomaly sink; a nil " +
			"one is a monitor with no queue, and every SEV1 it would have " +
			"raised is a nil dereference on the monitor goroutine")
	}

	r := &rig{cfg: c, ex: ex, anom: anom, snap: new(atomic.Pointer[risk.Snapshot])}
	var err error
	defer func() {
		if err != nil {
			r.unwind()
		}
	}()

	// (1) THE LOCK IS FIRST, and it is first with respect to the REST client
	// rather than merely with respect to the run loop. H-DEP-5's failure is two
	// processes on one account, and the damage begins at the first request each
	// of them sends -- so the second process must be refused before it can send
	// one. Nothing above this line touches the network.
	r.lock, err = lifecycle.AcquireInstanceLock(c.Paths.Lock)
	if err != nil {
		return nil, err
	}
	r.defer_(func() { r.lock.Close() })

	// (2) The store is OPENED. It is not created, and `provision.go` is where
	// creation lives. `hstore.StoreConfig.DBPath` says so in one line -- "It is
	// opened, never created fresh and never deleted" -- and the reason is
	// H-ORD-9: a database that is not there yet is indistinguishable, to
	// `hstore.Open`, from one that should be created, and the ledger it creates
	// recognises no order id at all. Every fill on the account then classifies
	// foreign, which is a SEV1 and a durable global stop about our own orders.
	if err = requireExistingDB(c.Paths.DB); err != nil {
		return nil, err
	}
	r.store, err = hstore.Open(hstore.StoreConfig{
		DBPath:         c.Paths.DB,
		AnomalyLogPath: c.Paths.AnomalyLog,
	})
	if err != nil {
		return nil, err
	}

	// (3) The writer runs before the first submission. `Store.submit` accepts a
	// record and returns a receipt whether or not anything is draining the
	// queue, so a submission made before this line is a record the caller
	// believes exists and nothing is writing.
	storeCtx, storeCancel := context.WithCancel(ctx)
	r.storeCancel = storeCancel
	r.storeDone = make(chan struct{})
	go func() {
		defer close(r.storeDone)
		r.store.Run(storeCtx)
	}()
	// The store is torn down through `Shutdown`, which is the only ordering
	// that does not destroy records; see `rig.close`. This unwind path is for a
	// CONSTRUCTION failure, where there is no orderly stop to run and the
	// records in flight are our own two.
	r.defer_(func() {
		storeCancel()
		<-r.storeDone
		r.store.Close()
	})

	// (4) The run row. §15 requires every §16 parameter recorded verbatim, and
	// the handle it issues is the licence every later record needs. It is
	// awaited rather than assumed: `BeginRun` returns a receipt, and the handle
	// arrives only through `TakeResults` once SQLite has committed.
	r.runID, err = newRunID()
	if err != nil {
		return nil, err
	}
	rcpt, err := r.store.BeginRun(r.runID, ex.NowMs(), c.Params)
	if err != nil {
		return nil, fmt.Errorf("recording the run row: %w", err)
	}
	r.run, r.deferred, err = awaitRunHandle(ctx, r.store, rcpt)
	if err != nil {
		return nil, err
	}

	// (5) The latch, through the controller that owns it. Constructing the
	// controller IS the read (H-HALT-4), and taking a `*GlobalController` is
	// what makes "read the latch before the first portfolio request" a type
	// requirement of `NewStartup` rather than an ordering someone remembers.
	r.latch, err = lifecycle.NewFileLatch(c.Paths.Latch)
	if err != nil {
		return nil, err
	}
	r.ctrl, r.boot, err = lifecycle.NewGlobalController(r.latch)
	if err != nil {
		return nil, err
	}
	r.anom.raiseAll(r.boot.Anomalies)
	if r.boot.Latched && !resume {
		// Assigned to `err` and not merely returned. The unwind closure reads
		// `err`, so a refusal that returns a fresh error without storing it
		// leaves this path holding the instance lock, the open store and a live
		// writer goroutine -- and the next start is then refused by its own
		// predecessor's lock, which reads as two harnesses running.
		err = fmt.Errorf("the durable halt latch at %s is SET, and "+
			"-resume was not given.\n\n"+
			"H-HALT-4 makes the latch survive the process on purpose: the "+
			"harness never self-clears it, so a restart into a latched state "+
			"is WINDING_DOWN and not RUNNING. §10.4 makes clearing it an "+
			"OPERATOR action, and this is where it becomes one. Read the "+
			"latch, understand why it was written, and pass -resume to start "+
			"a process that will wind the account down rather than quote it",
			c.Paths.Latch)
		return nil, err
	}

	// The exchange client. One `*rest.Client`, shared: it is stateless over the
	// `Doer`, and the single-writer property is a property of the DISPATCHER
	// goroutine (D3), not of the client object.
	//
	// THE GUARD GOES UNDER IT, not beside it (H-VER-1). Wrapping `ex.Doer` here
	// means startup's adoption sweep, the reducer, the dispatcher and anything
	// added later all reach the exchange through the same arming check, because
	// they all reach it through this one client. A guard installed at `Create`,
	// in `main`, or in the dispatcher would leave the other three able to write
	// -- and the startup sweep CANCELS orders, so "the dispatcher is guarded"
	// would still be a process that writes on boot.
	//
	// `M-ES6-NOGUARD` removes the wrapper and keeps everything else.
	arm := rest.WriteArm{Live: c.Live, LiveOKPath: c.Paths.LiveOK}
	guarded, err := rest.NewWriteGuard(ex.Doer, arm)
	if err != nil {
		return nil, err
	}
	r.api = rest.NewClient(guarded)

	tickers := []string{c.Ticker}

	r.guard, err = lifecycle.NewForeignGuard(r.store.Ownership())
	if err != nil {
		return nil, err
	}
	r.policy = newAdoptionPolicy(c.Params)
	r.start, err = lifecycle.NewStartup(r.ctrl, r.api, r.guard, r.policy,
		r.api, r.store, c.Params, tickers)
	if err != nil {
		return nil, err
	}

	r.gate, err = wsx.NewGate(tickers, c.Params)
	if err != nil {
		return nil, err
	}
	r.sup, err = wsx.NewSupervisor(ex.Signer, ex.Dialer, ex.Clock, c.Params,
		tickers)
	if err != nil {
		return nil, err
	}
	r.poll, err = wsx.NewPoller(r.api, ex.Clock, c.Params.PositionPoll)
	if err != nil {
		return nil, err
	}

	r.sigs, err = lifecycle.NewSignalController(r.ctrl)
	if err != nil {
		return nil, err
	}
	r.drain, err = lifecycle.NewDrainTracker(c.Params)
	if err != nil {
		return nil, err
	}

	r.book = core.NewRig(noopSink{}, map[string]float64{c.Ticker: ex.Target})
	r.pf = risk.NewPortfolio()
	r.queue = quote.NewQueue(c.Params.MaxQueueAge)
	r.cap = quote.NewCapacity(dispatchWorkers, c.Params.WriteBurst)

	r.mon = newMonitor(r.snap, c.Params.OwnerStall, ex.Mono,
		func(_ time.Duration, res risk.StepResult) {
			// The monitor's only outputs. Anomalies go to the buffered sink --
			// never a write, because I2 requires this goroutine to be
			// unstoppable by anything the quote engine or the disk does -- and
			// the samples are held for §13.3's heartbeat. There is no `snap`
			// table in §15's five, so they are not persisted.
			r.anom.raiseAll(res.Anomalies)
			held := res
			r.last.Store(&held)
		})

	return r, nil
}

// defer_ registers an unwind step for a construction failure.
func (r *rig) defer_(f func()) { r.cleanup = append(r.cleanup, f) }

// unwind runs the registered steps in reverse. It is the CONSTRUCTION failure
// path only; a running rig stops through `close`.
func (r *rig) unwind() {
	for i := len(r.cleanup) - 1; i >= 0; i-- {
		r.cleanup[i]()
	}
	r.cleanup = nil
}

// close is the orderly stop, and the order is `hstore.Shutdown`'s.
//
// It returns the store's refusal rather than forcing past it. A store that
// cannot drain is a store still retrying records it has ACCEPTED, and tearing
// it down converts "being written" into "permanently lost" -- so a caller that
// cannot stop cleanly is told, and the process stays up. The instance lock is
// released only after the store has been dealt with: while any record is still
// in flight this process is still the one incarnation entitled to write them.
func (r *rig) close(ctx context.Context) error {
	err := r.store.Shutdown(ctx, r.storeCancel)
	if err == nil {
		<-r.storeDone
	}
	if r.lock != nil {
		if cerr := r.lock.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// requireExistingDB is (2) above, stated as a refusal.
//
// A missing database is the ONE input to this process whose wrong value is
// silently accepted by everything downstream. `hstore.Open` creates a schema
// when the file is absent; the resulting ledger is valid, empty, and answers
// "not ours" for every order id on the account -- so §7.5's adoption classifies
// the harness's own resting orders as foreign activity, latches a durable global
// stop, and the operator is paged about a third party that does not exist.
//
// A typo in `paths.db` is exactly how that happens, and it is undetectable
// afterwards: the fresh file looks like a first run.
func requireExistingDB(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("no store at %s.\n\n"+
			"It is opened, never created by a run: an absent path and a "+
			"mistyped one are the same thing to this process, and the ledger "+
			"that would be created for either recognises no order id at all. "+
			"§7.5 would then read the harness's own resting orders as foreign "+
			"activity on a dedicated account, latch a durable global stop, and "+
			"page the operator about a third party that does not exist.\n\n"+
			"Create it deliberately: harness -provision -config <file>", path)
	case err != nil:
		return fmt.Errorf("store at %s could not be examined: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("store path %s is a directory", path)
	}
	return nil
}

// awaitRunHandle blocks until the `run` row commits.
//
// There is no way to shorten this and no reason to want to. The handle is
// `hstore`'s licence: it is issued by a committed row and by nothing else, so a
// harness that proceeded without one would be a harness whose every later record
// references a run that may not exist. Waiting here costs one SQLite commit,
// once, before the first exchange request.
//
// Results that are not ours are RETURNED, not discarded. At this point in
// construction there should be none -- one submission is outstanding -- but a
// terminal result is a record that is gone, and `hstore.Rejections` raises a
// SEV1 per lost record. Dropping one because it arrived early would lose exactly
// the evidence that says the store is already failing.
func awaitRunHandle(ctx context.Context, store *hstore.Store,
	rcpt hstore.Receipt) (hstore.RunHandle, []hstore.Result, error) {

	var others []hstore.Result
	for {
		for _, res := range store.TakeResults() {
			if res.Receipt.Seq() != rcpt.Seq() {
				others = append(others, res)
				continue
			}
			if res.Err != nil {
				return hstore.RunHandle{}, others, fmt.Errorf("the run row "+
					"could not be made durable, so there is no licence for any "+
					"record beneath it: %w", res.Err)
			}
			h, ok := res.RunHandle()
			if !ok {
				return hstore.RunHandle{}, others, errors.New("the run row " +
					"committed without issuing a handle; every record that " +
					"references run(run_id) needs one and there is no way to " +
					"construct one outside hstore")
			}
			return h, others, nil
		}
		select {
		case <-ctx.Done():
			return hstore.RunHandle{}, others, ctx.Err()
		case <-store.Wake():
		}
	}
}

// runIDBytes is the length of the random half of a run id.
//
// Eight base-32 characters is 40 bits. The id has to be unique among runs whose
// `owned_order` rows share one database, and a collision does not merely
// confuse a report: `BeginRun` refuses a repeat with different content, so the
// SECOND run of a colliding pair fails to start rather than corrupting the
// first. 40 bits makes that never happen; the timestamp prefix makes it
// impossible within a second even if it did.
const runIDBytes = 5

// runIDAlphabet is Crockford-style base 32 without the ambiguous letters.
// `rest.ValidRunID` permits alphanumerics only and forbids the "-" separator.
const runIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newRunID builds a sortable, greppable, separator-free run id.
//
// Sortable because the coid embeds it and a coid on the account is often the
// only thing an operator has to place an order in time. Separator-free because
// `ParseCoid` splits on "-" and a run id containing one shifts every field after
// it -- which would attribute an adopted order to the wrong market index, and
// therefore size a reducer against a position we do not hold.
func newRunID() (string, error) {
	var raw [runIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("run id randomness: %w", err)
	}
	id := time.Now().UTC().Format("20060102T150405")
	for _, b := range raw {
		id += string(runIDAlphabet[int(b)%len(runIDAlphabet)])
	}
	if err := rest.ValidRunID(id); err != nil {
		return "", err
	}
	return id, nil
}

// confidence: high
