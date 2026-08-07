package wsx

import (
	"context"
	"fmt"
	"time"

	"lip/harness/cfg"
)

// CommandKind is what a Command asks the live session to do.
type CommandKind uint8

const (
	// CmdUnknown is the zero value and is never valid. A zero-valued Command
	// arriving on the channel -- from a closed channel, or a struct someone
	// forgot to populate -- must not be executable as anything.
	CmdUnknown CommandKind = iota
	// CmdResnapshot is `update_subscription` with `action: get_snapshot`.
	CmdResnapshot
)

// Command is the only thing the owner may ask of the socket.
//
// The surface is one verb on purpose. Everything else the harness does to the
// exchange goes over REST, where a failure has a status code and a body; a
// command surface on the socket would put order flow behind a transport whose
// failure mode is "the frame may or may not have been sent".
type Command struct {
	Kind CommandKind
	// Sids are the subscription ids to resnapshot, in `core.Rig.Sids()` order.
	Sids []int64
	// Tickers is the market set to resnapshot.
	Tickers []string
}

// Validate rejects a command the exchange would answer with silence.
func (c Command) Validate() error {
	if c.Kind != CmdResnapshot {
		return fmt.Errorf("command kind %d is not a request this session "+
			"knows how to make", c.Kind)
	}
	return ValidateTickers(c.Tickers)
}

// EventKind is what happened on the socket.
type EventKind uint8

const (
	// EventUnknown is the zero value and is never emitted.
	EventUnknown EventKind = iota
	EventConnected
	EventFrame
	EventDisconnected
	// EventDisconnectReduce is F4: the socket has now been down for
	// disconnect_reduce_s. It is emitted ONCE per outage, while reconnect
	// attempts continue.
	EventDisconnectReduce
)

func (k EventKind) String() string {
	switch k {
	case EventConnected:
		return "connected"
	case EventFrame:
		return "frame"
	case EventDisconnected:
		return "disconnected"
	case EventDisconnectReduce:
		return "disconnect_reduce"
	}
	return "unknown"
}

// Event is one thing the supervisor observed.
type Event struct {
	Kind EventKind
	At   Stamp

	// Frame is the raw bytes, for EventFrame. Raw, because `core` is the
	// differentially tested decoder and putting a second one on this path
	// would make the tested one describe a different message.
	Frame []byte

	// Clean and Cause describe an EventDisconnected. Clean decides the BACKOFF
	// and nothing else -- H-FAIL-5 quarantines the book either way.
	Clean bool
	Cause error

	// Down is how long the socket has been down, for EventDisconnectReduce.
	Down time.Duration
}

// session is one connection's lifetime. It is created, run until it fails, and
// discarded; there is no reset path, because a reconnect also has to advance
// the gate's generation and reusing the object would invite forgetting that.
type session struct {
	sock Socket
	clk  Clock
	p    cfg.Params
}

// run drives one connection until it fails, and returns why.
//
// # The three-clock ladder (F1)
//
// Three independent timers, and each catches a failure the other two cannot:
//
//   - ping_interval_s -- we send a ping. Nothing detects anything by itself;
//     this is what MANUFACTURES evidence on an otherwise idle socket.
//   - pong_timeout_s -- the answer did not come. This catches a peer that is
//     gone while the TCP connection is still established, which is the
//     half-open case a read deadline alone takes ReadDeadline to notice.
//   - read_deadline_s -- NOTHING at all arrived, pongs included. This is the
//     backstop, and it exists because the pong path can succeed while the DATA
//     path is wedged: the library answers pings from inside its read loop, so a
//     server that is heartbeating and no longer publishing looks perfectly
//     healthy to the first two clocks.
//
// The read backstop is reset by DELIVERED DATA FRAMES only. Resetting it on
// pongs would collapse the third clock into the second and lose exactly the
// wedged-publisher case it is there for.
//
// `feed` deliberately has none of this: it reproduces `rig.py`'s
// `ping_interval=None` and its absent read deadline, because a measurement rig
// that reconnects where Python hangs produces rows Python never wrote. A
// harness that trades cannot make that trade.
func (s *session) run(ctx context.Context, cmds <-chan Command,
	events chan<- Event) error {

	frames := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			b, err := s.sock.Read(ctx)
			if err != nil {
				readErr <- err
				close(frames)
				return
			}
			select {
			case frames <- b:
			case <-ctx.Done():
				readErr <- ctx.Err()
				close(frames)
				return
			}
		}
	}()

	pingTimer := s.clk.NewTimer(s.p.WSPingInterval)
	defer pingTimer.Stop()
	readTimer := s.clk.NewTimer(s.p.ReadDeadline)
	defer readTimer.Stop()

	// At most one ping is outstanding. Its result arrives on a channel rather
	// than being waited on inline, so a pong timeout does not stall frame
	// delivery for the length of the timeout.
	//
	// The pong deadline is a timer on the INJECTED clock and not a
	// context.WithTimeout, which would read the real one. A deadline the
	// scenario exchange cannot move is a deadline no scenario can exercise,
	// and F1's whole ladder would then be untested at any speed a gate can
	// run at.
	pongs := make(chan error, 1)
	var (
		pongTimer  Timer
		pongCancel context.CancelFunc
	)
	defer func() {
		if pongTimer != nil {
			pongTimer.Stop()
		}
		if pongCancel != nil {
			pongCancel()
		}
	}()
	clearPing := func() {
		if pongTimer != nil {
			pongTimer.Stop()
			pongTimer = nil
		}
		if pongCancel != nil {
			pongCancel()
			pongCancel = nil
		}
	}

	for {
		// A nil channel blocks forever, so this is "only wait for a pong when
		// one is outstanding" with no extra state to keep in step.
		var pongDue <-chan time.Time
		if pongTimer != nil {
			pongDue = pongTimer.C()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-readErr:
			return err

		case b, ok := <-frames:
			if !ok {
				return <-readErr
			}
			// A delivered data frame is the only thing that proves the
			// publishing path is alive, so it is the only thing that clears the
			// backstop.
			readTimer.Reset(s.p.ReadDeadline)
			select {
			case events <- Event{Kind: EventFrame, At: s.clk.Now(), Frame: b}:
			case <-ctx.Done():
				return ctx.Err()
			}

		case <-pingTimer.C():
			pingTimer.Reset(s.p.WSPingInterval)
			if pongTimer != nil {
				// The previous ping has not been answered and its own deadline
				// has not expired yet. Sending a second one would reset the
				// library's expectation and hide the first one's silence.
				continue
			}
			pctx, cancel := context.WithCancel(ctx)
			pongCancel = cancel
			pongTimer = s.clk.NewTimer(s.p.PongTimeout)
			go func(c context.Context) { pongs <- s.sock.Ping(c) }(pctx)

		case err := <-pongs:
			clearPing()
			if err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}

		case <-pongDue:
			clearPing()
			return fmt.Errorf("no pong within pong_timeout_s %v: the peer is "+
				"gone while the connection is still established, which a read "+
				"deadline alone would take read_deadline_s to notice",
				s.p.PongTimeout)

		case <-readTimer.C():
			return fmt.Errorf("no data frame within read_deadline_s %v: the "+
				"socket is established and the publisher is silent, which "+
				"the ping ladder cannot see because pongs are answered from "+
				"inside the library's read loop", s.p.ReadDeadline)

		case c, ok := <-cmds:
			if !ok {
				cmds = nil
				continue
			}
			if err := c.Validate(); err != nil {
				// A malformed command is the caller's defect and does not
				// justify dropping a healthy socket.
				continue
			}
			b, err := resnapshotRequest(c.Sids, c.Tickers)
			if err != nil {
				continue
			}
			if err := s.sock.Write(ctx, b); err != nil {
				return fmt.Errorf("resnapshot write: %w", err)
			}
		}
	}
}

// confidence: high
