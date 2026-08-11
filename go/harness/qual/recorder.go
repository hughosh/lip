package qual

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lip/harness/rest"
)

// SchemaVersion is the only evidence schema this build reads or writes.
// Version 4 adds fixed-cadence, per-segment slot coverage. Earlier versions
// cannot be resumed without allowing callback bursts to stand in for later
// silence.
const SchemaVersion = 4

// MaxWouldWriteDetails is the hard bound on persisted decision fingerprints.
// Further decisions retain aggregate episode/observation counts in
// WouldWriteOverflow without growing the evidence file.
const MaxWouldWriteDetails = 256

var (
	// ErrCorrupt marks a checkpoint that is not strict, internally consistent
	// evidence.  Open never repairs or replaces such a file.
	ErrCorrupt = errors.New("qualification evidence is corrupt")
	// ErrMetadataMismatch marks a valid checkpoint belonging to a different
	// config, binary, ticker, rung, or arming mode.
	ErrMetadataMismatch = errors.New("qualification evidence metadata mismatch")
	// ErrFinalized marks an attempt to resume or update completed evidence.
	ErrFinalized = errors.New("qualification evidence is finalized")
)

// Metadata pins the identity and operating envelope of one qualification.
// Live must be false: this package's evidence format is specifically for the
// zero-write rung, not for a canary or a burn-in.
type Metadata struct {
	SchemaVersion  int    `json:"schema_version"`
	ConfigHash     string `json:"config_hash"`
	BinaryIdentity string `json:"binary_identity"`
	Ticker         string `json:"ticker"`
	Rung           string `json:"rung"`
	Live           bool   `json:"live"`
}

// SegmentStart identifies one OS process contributing to an evidence bundle.
// ID is caller-supplied so a process can give the segment a stable identity in
// logs as well as in the checkpoint.
type SegmentStart struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// ProcessSegment is one invocation. RunID binds the segment to a committed
// hstore run without coupling this evidence package to hstore. It is empty only
// during the pre-network interval between Open and LinkCurrentSegmentRun.
// An absent EndedAt means that invocation did not checkpoint a clean end before
// another process resumed the bundle. StartedAt and EndedAt are diagnostic wall
// stamps and need not be ordered; ActiveNanos is the only elapsed-time authority.
type ProcessSegment struct {
	ID             string                `json:"id"`
	RunID          string                `json:"run_id,omitempty"`
	PID            int                   `json:"pid"`
	StartedAt      time.Time             `json:"started_at"`
	EndedAt        *time.Time            `json:"ended_at,omitempty"`
	EndReason      string                `json:"end_reason,omitempty"`
	ActiveNanos    int64                 `json:"active_nanos"`
	MonitorSlots   MonitorSlotCoverage   `json:"monitor_slots"`
	PortfolioSlots PortfolioSlotCoverage `json:"portfolio_slots"`
}

// MonitorSlotCoverage counts distinct one-second cadence slots, not callback
// invocations. Repeated callbacks in one slot can improve that slot from stale
// to fresh, but can never fill a later silent slot.
type MonitorSlotCoverage struct {
	Observed uint64 `json:"observed"`
	Fresh    uint64 `json:"fresh"`
}

// PortfolioSlotCoverage counts distinct five-second cadence slots. StaleWalks
// and IncompleteWalks retain every observed bad walk, including duplicates in
// one slot, because q01 rejects either condition rather than averaging it away.
type PortfolioSlotCoverage struct {
	Observed        uint64 `json:"observed"`
	FreshComplete   uint64 `json:"fresh_complete"`
	StaleWalks      uint64 `json:"stale_walks"`
	IncompleteWalks uint64 `json:"incomplete_walks"`
}

// HTTPKey is a normalized below-guard transport operation. Method preserves
// exact spelling because the write guard's safe case is exact equality with
// "GET"; normalizing "get" into "GET" would hide a request the guard treats
// as a write.
type HTTPKey struct {
	Method   string `json:"method"`
	Endpoint string `json:"endpoint"`
}

// HTTPCount is the number of calls for one normalized endpoint and exact
// method spelling.
type HTTPCount struct {
	HTTPKey
	Count uint64 `json:"count"`
}

// WriteKind identifies the operation a decision would have attempted.
type WriteKind string

const (
	WriteCreate WriteKind = "create"
	WriteCancel WriteKind = "cancel"
)

// WouldWriteFingerprint contains every material field used to aggregate a
// would-write decision. Repeated owner-loop evaluations with identical fields
// in one segment update one detail even when another decision intervenes. A
// previously unseen price, quantity, state, target, side, or kind creates a new
// detail.
type WouldWriteFingerprint struct {
	Kind     WriteKind `json:"kind"`
	Ticker   string    `json:"ticker,omitempty"`
	Side     string    `json:"side,omitempty"`
	Price    string    `json:"price,omitempty"`
	Quantity string    `json:"quantity,omitempty"`
	OrderID  string    `json:"order_id,omitempty"`
	State    string    `json:"state,omitempty"`
}

// WouldWriteEpisode aggregates repeated observations of one decision within a
// process segment.
type WouldWriteEpisode struct {
	Fingerprint  WouldWriteFingerprint `json:"fingerprint"`
	SegmentID    string                `json:"segment_id"`
	FirstAt      time.Time             `json:"first_at"`
	LastAt       time.Time             `json:"last_at"`
	Observations uint64                `json:"observations"`
}

// WouldWriteOverflow is the bounded remainder after MaxWouldWriteDetails
// distinct segment/fingerprint details have been retained.  Observations is
// exact. Episodes counts contiguous overflow runs; the detail is deliberately
// absent rather than silently dropped.
type WouldWriteOverflow struct {
	Episodes     uint64 `json:"episodes"`
	Observations uint64 `json:"observations"`
}

// FreshnessCounters summarize monitor samples whose source did or did not
// advance.  Check timestamps are observation times supplied by the caller.
type FreshnessCounters struct {
	Checks      uint64    `json:"checks"`
	Fresh       uint64    `json:"fresh"`
	Stale       uint64    `json:"stale"`
	LastCheckAt time.Time `json:"last_check_at,omitempty"`
	LastFreshAt time.Time `json:"last_fresh_at,omitempty"`
}

// PortfolioCounters distinguish freshness from completeness.  FreshComplete
// is the qualification-useful intersection rather than an inference from two
// unrelated totals.
type PortfolioCounters struct {
	Walks          uint64    `json:"walks"`
	Fresh          uint64    `json:"fresh"`
	Stale          uint64    `json:"stale"`
	Complete       uint64    `json:"complete"`
	Incomplete     uint64    `json:"incomplete"`
	FreshComplete  uint64    `json:"fresh_complete"`
	LastWalkAt     time.Time `json:"last_walk_at,omitempty"`
	LastFreshAt    time.Time `json:"last_fresh_at,omitempty"`
	LastCompleteAt time.Time `json:"last_complete_at,omitempty"`
}

// EventCategory is one of the generic evidence channels.  Event names remain
// caller-defined so adding a forced scenario or state does not change the
// evidence schema.
type EventCategory string

const (
	EventForced    EventCategory = "forced"
	EventState     EventCategory = "state"
	EventAnomaly   EventCategory = "anomaly"
	EventHeartbeat EventCategory = "heartbeat"
)

// EventCounter aggregates occurrences of a named event.
type EventCounter struct {
	Category EventCategory `json:"category"`
	Name     string        `json:"name"`
	Count    uint64        `json:"count"`
	FirstAt  time.Time     `json:"first_at"`
	LastAt   time.Time     `json:"last_at"`
}

// Evidence is a point-in-time, detached view suitable for JSON inspection.
// Snapshot returns slices in stable order.
type Evidence struct {
	Metadata  Metadata         `json:"metadata"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	Segments  []ProcessSegment `json:"segments"`
	// AttemptedHTTP is observed above WriteGuard. HTTP is observed below it,
	// immediately before the network transport.
	AttemptedHTTP      []HTTPCount         `json:"attempted_http"`
	HTTP               []HTTPCount         `json:"http"`
	WouldWrites        []WouldWriteEpisode `json:"would_writes"`
	WouldWriteOverflow WouldWriteOverflow  `json:"would_write_overflow"`
	Monitor            FreshnessCounters   `json:"monitor"`
	Portfolio          PortfolioCounters   `json:"portfolio"`
	Events             []EventCounter      `json:"events"`
	FinalizedAt        *time.Time          `json:"finalized_at,omitempty"`
}

type eventKey struct {
	category EventCategory
	name     string
}

type wouldWriteKey struct {
	segmentID   string
	fingerprint WouldWriteFingerprint
}

// Recorder is safe for concurrent owner, monitor, ping, and REST calls.
type Recorder struct {
	// checkpointMu serializes durable replacements so an older snapshot can
	// never rename over a newer one.  mu is held only long enough to capture
	// the snapshot; JSON encoding and filesystem I/O happen without blocking
	// observation producers.
	checkpointMu sync.Mutex
	mu           sync.Mutex

	path              string
	evidence          Evidence
	attemptHTTPCounts map[HTTPKey]uint64
	httpCounts        map[HTTPKey]uint64
	wouldWrites       []WouldWriteEpisode
	wouldIndex        map[wouldWriteKey]int
	overflowLast      wouldWriteKey
	overflowLastSet   bool
	events            map[eventKey]EventCounter
	currentSegment    int
	now               func() time.Time
	activeOrigin      time.Time
	activeNow         func() time.Time
	monitorSlot       uint64
	monitorSlotSet    bool
	monitorSlotFresh  bool
	portfolioSlot     uint64
	portfolioSlotSet  bool
	portfolioSlotGood bool
	ops               atomicFileOps
}

// Open creates or strictly resumes path, then durably appends segment.  A
// corrupt, finalized, differently configured, or differently built checkpoint
// is left byte-for-byte untouched and refused.
func Open(path string, metadata Metadata, segment SegmentStart) (*Recorder, error) {
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	segment = normalizeSegmentStart(segment)
	if err := validateSegmentStart(segment); err != nil {
		return nil, err
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("qualification evidence path %q is not absolute", path)
	}

	activeOrigin := time.Now()
	r := &Recorder{
		path:              path,
		attemptHTTPCounts: make(map[HTTPKey]uint64),
		httpCounts:        make(map[HTTPKey]uint64),
		wouldIndex:        make(map[wouldWriteKey]int),
		events:            make(map[eventKey]EventCounter),
		currentSegment:    -1,
		now:               time.Now,
		activeOrigin:      activeOrigin,
		activeNow:         time.Now,
		ops:               realAtomicFileOps(),
	}

	loaded, err := readEvidence(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.evidence = Evidence{
			Metadata:  metadata,
			CreatedAt: segment.StartedAt,
			UpdatedAt: segment.StartedAt,
		}
	case err != nil:
		return nil, err
	default:
		if loaded.FinalizedAt != nil {
			return nil, ErrFinalized
		}
		if loaded.Metadata != metadata {
			return nil, fmt.Errorf("%w: checkpoint has %+v, process has %+v",
				ErrMetadataMismatch, loaded.Metadata, metadata)
		}
		if err := r.load(loaded); err != nil {
			return nil, err
		}
	}

	for _, existing := range r.evidence.Segments {
		if existing.ID == segment.ID {
			return nil, fmt.Errorf("segment id %q already exists", segment.ID)
		}
	}
	r.evidence.Segments = append(r.evidence.Segments, ProcessSegment{
		ID: segment.ID, PID: segment.PID, StartedAt: segment.StartedAt,
	})
	r.currentSegment = len(r.evidence.Segments) - 1
	r.touchLocked(segment.StartedAt)
	if err := r.writeSnapshot(r.snapshotLocked()); err != nil {
		return nil, fmt.Errorf("checkpointing process segment %q: %w", segment.ID, err)
	}
	return r, nil
}

// LinkCurrentSegmentRun durably binds the current process segment to the
// caller's committed hstore run. A repeated link to the same run is idempotent;
// an empty or different run cannot replace the first authority. Committing the
// link resets the active clock and cadence slots, so construction before the
// run authority exists cannot qualify as runtime.
func (r *Recorder) LinkCurrentSegmentRun(runID string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("qualification run id is empty")
	}

	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()

	r.mu.Lock()
	if err := r.mutableLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	segment := &r.evidence.Segments[r.currentSegment]
	if segment.RunID != "" {
		r.mu.Unlock()
		if segment.RunID == runID {
			return nil
		}
		return fmt.Errorf("process segment %q is already linked to run %q",
			segment.ID, segment.RunID)
	}
	oldActive := segment.ActiveNanos
	oldMonitor := segment.MonitorSlots
	oldPortfolio := segment.PortfolioSlots
	segment.RunID = runID
	segment.ActiveNanos = 0
	segment.MonitorSlots = MonitorSlotCoverage{}
	segment.PortfolioSlots = PortfolioSlotCoverage{}
	snapshot := r.snapshotLocked()
	// Do not expose authority in memory before it is durable. Observations made
	// while the replacement is in flight remain pre-authority and therefore do
	// not enter the per-segment cadence evidence.
	segment.RunID = ""
	segment.ActiveNanos = oldActive
	segment.MonitorSlots = oldMonitor
	segment.PortfolioSlots = oldPortfolio
	r.mu.Unlock()

	if err := r.writeSnapshot(snapshot); err != nil {
		return fmt.Errorf("checkpointing run link for process segment %q: %w",
			segment.ID, err)
	}
	r.mu.Lock()
	segment = &r.evidence.Segments[r.currentSegment]
	segment.RunID = runID
	segment.ActiveNanos = 0
	segment.MonitorSlots = MonitorSlotCoverage{}
	segment.PortfolioSlots = PortfolioSlotCoverage{}
	r.activeOrigin = r.activeNow()
	r.monitorSlotSet = false
	r.monitorSlotFresh = false
	r.portfolioSlotSet = false
	r.portfolioSlotGood = false
	r.mu.Unlock()
	return nil
}

// WrapAttemptDoer records requests above WriteGuard. Compose it outside the
// guard, with WrapDoer inside, so refused writes appear only in AttemptedHTTP
// while requests that can reach the network also appear in HTTP.
func (r *Recorder) WrapAttemptDoer(next rest.Doer) (rest.Doer, error) {
	if next == nil {
		return nil, errors.New("qualification attempted-HTTP recorder needs a Doer to wrap")
	}
	return &attemptRecordingDoer{recorder: r, next: next}, nil
}

type attemptRecordingDoer struct {
	recorder *Recorder
	next     rest.Doer
}

func (d *attemptRecordingDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if err := d.recorder.recordAttemptHTTP(req, d.recorder.now(), req.Method != "GET"); err != nil {
		return rest.Response{}, err
	}
	return d.next.Do(ctx, req)
}

// WrapDoer records calls that reach next.  To make these below-guard counts,
// pass the result as the next Doer to rest.NewWriteGuard; reversing that order
// counts refused would-writes as transport calls and invalidates the evidence.
func (r *Recorder) WrapDoer(next rest.Doer) (rest.Doer, error) {
	if next == nil {
		return nil, errors.New("qualification HTTP recorder needs a Doer to wrap")
	}
	return &recordingDoer{recorder: r, next: next}, nil
}

type recordingDoer struct {
	recorder *Recorder
	next     rest.Doer
}

func (d *recordingDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if err := d.recorder.recordHTTP(req, d.recorder.now(), req.Method != "GET"); err != nil {
		return rest.Response{}, err
	}
	return d.next.Do(ctx, req)
}

func (r *Recorder) recordAttemptHTTP(req rest.Request, at time.Time, durable bool) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	method := req.Method
	if method == "" {
		method = "UNKNOWN"
	}
	key := HTTPKey{Method: method, Endpoint: NormalizeEndpoint(req.Path)}

	if durable {
		r.checkpointMu.Lock()
		defer r.checkpointMu.Unlock()
	}
	r.mu.Lock()
	if err := r.mutableLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.attemptHTTPCounts[key]++
	r.touchLocked(at)
	if !durable {
		r.mu.Unlock()
		return nil
	}
	if err := r.updateActiveLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()
	if err := r.writeSnapshot(snapshot); err != nil {
		return fmt.Errorf("durably recording above-guard attempted %s %s before forwarding: %w",
			key.Method, key.Endpoint, err)
	}
	return nil
}

// NormalizeEndpoint removes query text, cleans redundant separators, and
// replaces the two route parameters used by the harness.  It never guesses
// that an arbitrary path component is an identifier.
func NormalizeEndpoint(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	clean := pathpkg.Clean("/" + strings.TrimPrefix(raw, "/"))
	if strings.HasPrefix(clean, "/portfolio/events/orders/") {
		return "/portfolio/events/orders/{order_id}"
	}
	if strings.HasPrefix(clean, "/markets/") {
		return "/markets/{ticker}"
	}
	return clean
}

func (r *Recorder) recordHTTP(req rest.Request, at time.Time, durable bool) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	method := req.Method
	if method == "" {
		method = "UNKNOWN"
	}
	key := HTTPKey{Method: method, Endpoint: NormalizeEndpoint(req.Path)}

	if durable {
		r.checkpointMu.Lock()
		defer r.checkpointMu.Unlock()
	}
	r.mu.Lock()
	if err := r.mutableLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.httpCounts[key]++
	r.touchLocked(at)
	if !durable {
		r.mu.Unlock()
		return nil
	}
	if err := r.updateActiveLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()
	if err := r.writeSnapshot(snapshot); err != nil {
		return fmt.Errorf("durably recording below-guard %s %s before forwarding: %w",
			key.Method, key.Endpoint, err)
	}
	return nil
}

// RecordWouldWrite observes a pre-guard decision. Equal fingerprints within a
// process segment are aggregated even when separated by another decision, so
// A, A, B, A retains two bounded details with observation counts three and
// one. A restart begins a new detail because SegmentID is part of the key.
func (r *Recorder) RecordWouldWrite(f WouldWriteFingerprint, at time.Time) error {
	if err := validateFingerprint(f); err != nil {
		return err
	}
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	segmentID := r.evidence.Segments[r.currentSegment].ID
	key := wouldWriteKey{segmentID: segmentID, fingerprint: f}
	if i, ok := r.wouldIndex[key]; ok {
		episode := &r.wouldWrites[i]
		if at.Before(episode.FirstAt) {
			episode.FirstAt = at
		}
		if at.After(episode.LastAt) {
			episode.LastAt = at
		}
		episode.Observations++
		r.overflowLastSet = false
	} else if len(r.wouldWrites) < MaxWouldWriteDetails {
		r.wouldWrites = append(r.wouldWrites, WouldWriteEpisode{
			Fingerprint:  f,
			SegmentID:    segmentID,
			FirstAt:      at,
			LastAt:       at,
			Observations: 1,
		})
		r.wouldIndex[key] = len(r.wouldWrites) - 1
		r.overflowLastSet = false
	} else {
		overflow := &r.evidence.WouldWriteOverflow
		if !r.overflowLastSet || r.overflowLast != key {
			overflow.Episodes++
		}
		overflow.Observations++
		r.overflowLast = key
		r.overflowLastSet = true
	}
	r.touchLocked(at)
	return nil
}

// RecordMonitorSample records whether the monitor consumed a source-advanced
// sample; a freshly timestamped observation of a frozen source is stale.
func (r *Recorder) RecordMonitorSample(fresh bool, at time.Time) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	segment := &r.evidence.Segments[r.currentSegment]
	var slot uint64
	if segment.RunID != "" {
		elapsed, err := r.activeElapsedLocked()
		if err != nil {
			return err
		}
		slot = cadenceSlot(elapsed, time.Second)
		if r.monitorSlotSet && slot < r.monitorSlot {
			return fmt.Errorf("monitor cadence slot moved backwards from %d to %d",
				r.monitorSlot, slot)
		}
	}
	c := &r.evidence.Monitor
	c.Checks++
	if at.After(c.LastCheckAt) {
		c.LastCheckAt = at
	}
	if fresh {
		c.Fresh++
		if at.After(c.LastFreshAt) {
			c.LastFreshAt = at
		}
	} else {
		c.Stale++
	}
	if segment.RunID != "" {
		switch {
		case !r.monitorSlotSet || slot > r.monitorSlot:
			segment.MonitorSlots.Observed++
			if fresh {
				segment.MonitorSlots.Fresh++
			}
			r.monitorSlot = slot
			r.monitorSlotSet = true
			r.monitorSlotFresh = fresh
		case fresh && !r.monitorSlotFresh:
			segment.MonitorSlots.Fresh++
			r.monitorSlotFresh = true
		}
	}
	r.touchLocked(at)
	return nil
}

// RecordPortfolioWalk records both independent properties of one complete API
// walk: source freshness and whether every page completed successfully.
func (r *Recorder) RecordPortfolioWalk(fresh, complete bool, at time.Time) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	segment := &r.evidence.Segments[r.currentSegment]
	var slot uint64
	if segment.RunID != "" {
		elapsed, err := r.activeElapsedLocked()
		if err != nil {
			return err
		}
		slot = cadenceSlot(elapsed, 5*time.Second)
		if r.portfolioSlotSet && slot < r.portfolioSlot {
			return fmt.Errorf("portfolio cadence slot moved backwards from %d to %d",
				r.portfolioSlot, slot)
		}
	}
	c := &r.evidence.Portfolio
	c.Walks++
	if at.After(c.LastWalkAt) {
		c.LastWalkAt = at
	}
	if fresh {
		c.Fresh++
		if at.After(c.LastFreshAt) {
			c.LastFreshAt = at
		}
	} else {
		c.Stale++
	}
	if complete {
		c.Complete++
		if at.After(c.LastCompleteAt) {
			c.LastCompleteAt = at
		}
	} else {
		c.Incomplete++
	}
	if fresh && complete {
		c.FreshComplete++
	}
	if segment.RunID != "" {
		good := fresh && complete
		switch {
		case !r.portfolioSlotSet || slot > r.portfolioSlot:
			segment.PortfolioSlots.Observed++
			if good {
				segment.PortfolioSlots.FreshComplete++
			}
			r.portfolioSlot = slot
			r.portfolioSlotSet = true
			r.portfolioSlotGood = good
		case good && !r.portfolioSlotGood:
			segment.PortfolioSlots.FreshComplete++
			r.portfolioSlotGood = true
		}
		if !fresh {
			segment.PortfolioSlots.StaleWalks++
		}
		if !complete {
			segment.PortfolioSlots.IncompleteWalks++
		}
	}
	r.touchLocked(at)
	return nil
}

// RecordEvent increments a named forced-action, state, anomaly, or heartbeat
// counter.  Names are deliberately open vocabulary; categories are not.
func (r *Recorder) RecordEvent(category EventCategory, name string, at time.Time) error {
	if !validEventCategory(category) {
		return fmt.Errorf("unknown qualification event category %q", category)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("qualification event name is empty")
	}
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	key := eventKey{category: category, name: name}
	event, ok := r.events[key]
	if !ok {
		event = EventCounter{Category: category, Name: name, FirstAt: at}
	} else if at.Before(event.FirstAt) {
		event.FirstAt = at
	}
	event.Count++
	if event.LastAt.IsZero() || at.After(event.LastAt) {
		event.LastAt = at
	}
	r.events[key] = event
	r.touchLocked(at)
	return nil
}

// EndSegment marks the current process invocation as cleanly ended.  Call
// Checkpoint afterwards, or use Finalize when the whole qualification is done.
func (r *Recorder) EndSegment(at time.Time, reason string) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("process segment end reason is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.mutableLocked(); err != nil {
		return err
	}
	segment := &r.evidence.Segments[r.currentSegment]
	if segment.EndedAt != nil {
		return fmt.Errorf("process segment %q is already ended", segment.ID)
	}
	if err := r.updateActiveLocked(); err != nil {
		return err
	}
	segment.EndedAt = timePointer(at)
	segment.EndReason = reason
	r.touchLocked(at)
	return nil
}

// Snapshot returns a detached, stable-order view of all observations.
func (r *Recorder) Snapshot() Evidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// Checkpoint atomically replaces the mode-0600 JSON file after syncing the
// temporary file and, after rename, its parent directory.
func (r *Recorder) Checkpoint() error {
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()

	r.mu.Lock()
	if err := r.mutableLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	if err := r.updateActiveLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	snapshot := r.snapshotLocked()
	r.mu.Unlock()
	return r.writeSnapshot(snapshot)
}

// Finalize cleanly ends the current segment if necessary, marks the bundle
// immutable, and checkpoints it.  If replacement fails before rename, the
// recorder remains mutable so the caller can retry.
func (r *Recorder) Finalize(at time.Time) error {
	at, err := observationTime(at)
	if err != nil {
		return err
	}
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()

	r.mu.Lock()
	if err := r.mutableLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	old := r.evidence
	old.Segments = append([]ProcessSegment(nil), r.evidence.Segments...)
	segment := &r.evidence.Segments[r.currentSegment]
	if err := r.updateActiveLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	if segment.EndedAt == nil {
		segment.EndedAt = timePointer(at)
		segment.EndReason = "finalized"
	}
	r.evidence.FinalizedAt = timePointer(at)
	r.touchLocked(at)
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	if err := r.writeSnapshot(snapshot); err != nil {
		r.mu.Lock()
		r.evidence = old
		r.mu.Unlock()
		return err
	}
	return nil
}

func (r *Recorder) mutableLocked() error {
	if r.evidence.FinalizedAt != nil {
		return ErrFinalized
	}
	return nil
}

func (r *Recorder) touchLocked(at time.Time) {
	if at.After(r.evidence.UpdatedAt) {
		r.evidence.UpdatedAt = at
	}
}

func (r *Recorder) updateActiveLocked() error {
	segment := &r.evidence.Segments[r.currentSegment]
	if segment.EndedAt != nil || segment.RunID == "" {
		return nil
	}
	elapsed, err := r.activeElapsedLocked()
	if err != nil {
		return err
	}
	if elapsed.Nanoseconds() > segment.ActiveNanos {
		segment.ActiveNanos = elapsed.Nanoseconds()
	}
	return nil
}

func (r *Recorder) activeElapsedLocked() (time.Duration, error) {
	elapsed := r.activeNow().Sub(r.activeOrigin)
	if elapsed < 0 {
		return 0, fmt.Errorf("qualification monotonic clock moved backwards by %s", -elapsed)
	}
	return elapsed, nil
}

func cadenceSlot(elapsed, cadence time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	// The right edge belongs to the interval it closes: (0, cadence] is slot
	// zero, (cadence, 2*cadence] is slot one. This agrees exactly with the
	// ceiling division used to derive expected slots from ActiveNanos.
	return uint64((elapsed - 1) / cadence)
}

func (r *Recorder) snapshotLocked() Evidence {
	out := r.evidence
	out.Segments = append([]ProcessSegment(nil), r.evidence.Segments...)
	for i := range out.Segments {
		if out.Segments[i].EndedAt != nil {
			out.Segments[i].EndedAt = timePointer(*out.Segments[i].EndedAt)
		}
	}
	if r.evidence.FinalizedAt != nil {
		out.FinalizedAt = timePointer(*r.evidence.FinalizedAt)
	}

	out.AttemptedHTTP = sortedHTTPCounts(r.attemptHTTPCounts)
	out.HTTP = sortedHTTPCounts(r.httpCounts)

	// Would-write details retain first-encounter order. Aggregation is by
	// segment/fingerprint, so later returns to an earlier decision update its
	// existing position rather than appending another detail.
	out.WouldWrites = append([]WouldWriteEpisode(nil), r.wouldWrites...)

	out.Events = make([]EventCounter, 0, len(r.events))
	for _, event := range r.events {
		out.Events = append(out.Events, event)
	}
	sort.Slice(out.Events, func(i, j int) bool {
		if out.Events[i].Category != out.Events[j].Category {
			return out.Events[i].Category < out.Events[j].Category
		}
		return out.Events[i].Name < out.Events[j].Name
	})
	return out
}

func sortedHTTPCounts(counts map[HTTPKey]uint64) []HTTPCount {
	out := make([]HTTPCount, 0, len(counts))
	for key, count := range counts {
		out = append(out, HTTPCount{HTTPKey: key, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out
}

func (r *Recorder) writeSnapshot(snapshot Evidence) error {
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal qualification evidence: %w", err)
	}
	b = append(b, '\n')
	if err := atomicWriteFile(r.path, b, r.ops); err != nil {
		return fmt.Errorf("checkpoint qualification evidence: %w", err)
	}
	return nil
}

func (r *Recorder) load(e Evidence) error {
	if err := validateEvidence(e); err != nil {
		return err
	}
	r.evidence = e
	r.evidence.AttemptedHTTP = nil
	r.evidence.HTTP = nil
	r.evidence.WouldWrites = nil
	r.evidence.Events = nil
	for _, count := range e.AttemptedHTTP {
		r.attemptHTTPCounts[count.HTTPKey] = count.Count
	}
	for _, count := range e.HTTP {
		r.httpCounts[count.HTTPKey] = count.Count
	}
	r.wouldWrites = append([]WouldWriteEpisode(nil), e.WouldWrites...)
	for i, episode := range r.wouldWrites {
		r.wouldIndex[wouldWriteKey{
			segmentID: episode.SegmentID, fingerprint: episode.Fingerprint,
		}] = i
	}
	for _, event := range e.Events {
		r.events[eventKey{category: event.Category, name: event.Name}] = event
	}
	return nil
}

// LoadEvidence reads one preserved bundle for offline inspection, using the
// same strict reader Open resumes with: exact mode, one JSON value, no unknown
// fields, and full structural validation.  It is deliberately read-only and
// takes no Recorder: an assessor must never be able to append a segment,
// finalize, or otherwise alter the evidence it is judging.
func LoadEvidence(path string) (Evidence, error) {
	if path == "" || !filepath.IsAbs(path) {
		return Evidence{}, fmt.Errorf("qualification evidence path %q is not absolute", path)
	}
	return readEvidence(path)
}

func readEvidence(path string) (Evidence, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Evidence{}, err
	}
	if !info.Mode().IsRegular() {
		return Evidence{}, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, path)
	}
	if info.Mode().Perm() != 0o600 {
		return Evidence{}, fmt.Errorf("%w: %s mode is %04o, want 0600",
			ErrCorrupt, path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Evidence{}, fmt.Errorf("read qualification evidence: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var e Evidence
	if err := decoder.Decode(&e); err != nil {
		return Evidence{}, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Evidence{}, fmt.Errorf("%w: trailing data: %v", ErrCorrupt, err)
	}
	if err := validateEvidence(e); err != nil {
		return Evidence{}, err
	}
	return e, nil
}

func validateMetadata(m Metadata) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("qualification schema %d, this build requires %d",
			m.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(m.ConfigHash) == "" {
		return errors.New("qualification metadata config hash is empty")
	}
	if strings.TrimSpace(m.BinaryIdentity) == "" {
		return errors.New("qualification metadata binary identity is empty")
	}
	if strings.TrimSpace(m.Ticker) == "" {
		return errors.New("qualification metadata ticker is empty")
	}
	if strings.TrimSpace(m.Rung) == "" {
		return errors.New("qualification metadata rung is empty")
	}
	if m.Live {
		return errors.New("qualification evidence requires live=false")
	}
	return nil
}

func validateSegmentStart(s SegmentStart) error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("qualification process segment id is empty")
	}
	if s.PID <= 0 {
		return fmt.Errorf("qualification process segment pid %d is not positive", s.PID)
	}
	if s.StartedAt.IsZero() {
		return errors.New("qualification process segment start time is zero")
	}
	return nil
}

func normalizeSegmentStart(s SegmentStart) SegmentStart {
	s.ID = strings.TrimSpace(s.ID)
	s.StartedAt = s.StartedAt.UTC()
	return s
}

func validateFingerprint(f WouldWriteFingerprint) error {
	switch f.Kind {
	case WriteCreate:
		if strings.TrimSpace(f.Ticker) == "" || strings.TrimSpace(f.Side) == "" ||
			strings.TrimSpace(f.Price) == "" || strings.TrimSpace(f.Quantity) == "" {
			return errors.New("create would-write fingerprint needs ticker, side, price, and quantity")
		}
	case WriteCancel:
		if strings.TrimSpace(f.OrderID) == "" {
			return errors.New("cancel would-write fingerprint needs order id")
		}
	default:
		return fmt.Errorf("unknown would-write kind %q", f.Kind)
	}
	return nil
}

func validateEvidence(e Evidence) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
	}
	if err := validateMetadata(e.Metadata); err != nil {
		return bad("metadata: %v", err)
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		return bad("created/updated timestamp is zero")
	}
	if len(e.Segments) == 0 {
		return bad("no process segments")
	}
	segmentIDs := make(map[string]bool, len(e.Segments))
	var monitorSlots MonitorSlotCoverage
	var portfolioSlots PortfolioSlotCoverage
	for _, segment := range e.Segments {
		if err := validateSegmentStart(SegmentStart{
			ID: segment.ID, PID: segment.PID, StartedAt: segment.StartedAt,
		}); err != nil {
			return bad("segment: %v", err)
		}
		if segmentIDs[segment.ID] {
			return bad("duplicate segment id %q", segment.ID)
		}
		segmentIDs[segment.ID] = true
		if segment.EndedAt != nil {
			if segment.EndedAt.IsZero() || strings.TrimSpace(segment.EndReason) == "" {
				return bad("segment %q has invalid end", segment.ID)
			}
		} else if segment.EndReason != "" {
			return bad("segment %q has an end reason but no end time", segment.ID)
		}
		if segment.ActiveNanos < 0 {
			return bad("segment %q has negative active duration %d", segment.ID, segment.ActiveNanos)
		}
		if segment.MonitorSlots.Fresh > segment.MonitorSlots.Observed {
			return bad("segment %q monitor fresh slots exceed observed slots", segment.ID)
		}
		if segment.PortfolioSlots.FreshComplete > segment.PortfolioSlots.Observed {
			return bad("segment %q portfolio fresh+complete slots exceed observed slots",
				segment.ID)
		}
		monitorSlots.Observed += segment.MonitorSlots.Observed
		monitorSlots.Fresh += segment.MonitorSlots.Fresh
		portfolioSlots.Observed += segment.PortfolioSlots.Observed
		portfolioSlots.FreshComplete += segment.PortfolioSlots.FreshComplete
		portfolioSlots.StaleWalks += segment.PortfolioSlots.StaleWalks
		portfolioSlots.IncompleteWalks += segment.PortfolioSlots.IncompleteWalks
	}
	if e.FinalizedAt != nil && e.FinalizedAt.IsZero() {
		return bad("finalization timestamp is zero")
	}
	validateHTTP := func(scope string, counts []HTTPCount) error {
		httpKeys := make(map[HTTPKey]bool, len(counts))
		for _, count := range counts {
			if count.Count == 0 || count.Method == "" || count.Endpoint == "" {
				return bad("invalid %s HTTP count %+v", scope, count)
			}
			if count.Endpoint != NormalizeEndpoint(count.Endpoint) {
				return bad("non-normalized %s HTTP key %+v", scope, count.HTTPKey)
			}
			if httpKeys[count.HTTPKey] {
				return bad("duplicate %s HTTP key %+v", scope, count.HTTPKey)
			}
			httpKeys[count.HTTPKey] = true
		}
		return nil
	}
	if err := validateHTTP("attempted", e.AttemptedHTTP); err != nil {
		return err
	}
	if err := validateHTTP("below-guard", e.HTTP); err != nil {
		return err
	}

	if len(e.WouldWrites) > MaxWouldWriteDetails {
		return bad("%d would-write details exceed hard bound %d",
			len(e.WouldWrites), MaxWouldWriteDetails)
	}
	wouldKeys := make(map[wouldWriteKey]bool, len(e.WouldWrites))
	for _, episode := range e.WouldWrites {
		if err := validateFingerprint(episode.Fingerprint); err != nil {
			return bad("would-write fingerprint: %v", err)
		}
		if !segmentIDs[episode.SegmentID] {
			return bad("would-write episode names unknown segment %q", episode.SegmentID)
		}
		if episode.Observations == 0 || episode.FirstAt.IsZero() ||
			episode.LastAt.Before(episode.FirstAt) {
			return bad("invalid would-write episode %+v", episode)
		}
		key := wouldWriteKey{segmentID: episode.SegmentID, fingerprint: episode.Fingerprint}
		if wouldKeys[key] {
			return bad("duplicate would-write detail in segment %q for fingerprint %+v",
				episode.SegmentID, episode.Fingerprint)
		}
		wouldKeys[key] = true
	}
	if e.WouldWriteOverflow.Observations == 0 && e.WouldWriteOverflow.Episodes != 0 {
		return bad("would-write overflow has episodes without observations")
	}
	if e.WouldWriteOverflow.Observations != 0 && e.WouldWriteOverflow.Episodes == 0 {
		return bad("would-write overflow has observations without episodes")
	}
	if e.WouldWriteOverflow.Observations != 0 && len(e.WouldWrites) != MaxWouldWriteDetails {
		return bad("would-write overflow exists before the detail bound was reached")
	}
	if e.WouldWriteOverflow.Episodes > e.WouldWriteOverflow.Observations {
		return bad("would-write overflow episodes exceed observations")
	}

	if e.Monitor.Checks != e.Monitor.Fresh+e.Monitor.Stale {
		return bad("monitor freshness counts do not sum")
	}
	if monitorSlots.Observed > e.Monitor.Checks || monitorSlots.Fresh > e.Monitor.Fresh {
		return bad("per-segment monitor slots exceed callback totals")
	}
	if e.Portfolio.Walks != e.Portfolio.Fresh+e.Portfolio.Stale ||
		e.Portfolio.Walks != e.Portfolio.Complete+e.Portfolio.Incomplete ||
		e.Portfolio.FreshComplete > e.Portfolio.Fresh ||
		e.Portfolio.FreshComplete > e.Portfolio.Complete {
		return bad("portfolio freshness/completeness counts do not sum")
	}
	if portfolioSlots.Observed > e.Portfolio.Walks ||
		portfolioSlots.FreshComplete > e.Portfolio.FreshComplete ||
		portfolioSlots.StaleWalks > e.Portfolio.Stale ||
		portfolioSlots.IncompleteWalks > e.Portfolio.Incomplete {
		return bad("per-segment portfolio slots exceed callback totals")
	}

	eventKeys := make(map[eventKey]bool, len(e.Events))
	for _, event := range e.Events {
		if !validEventCategory(event.Category) || strings.TrimSpace(event.Name) == "" ||
			event.Count == 0 || event.FirstAt.IsZero() || event.LastAt.Before(event.FirstAt) {
			return bad("invalid event %+v", event)
		}
		key := eventKey{category: event.Category, name: event.Name}
		if eventKeys[key] {
			return bad("duplicate event %s/%s", event.Category, event.Name)
		}
		eventKeys[key] = true
	}
	return nil
}

func validEventCategory(category EventCategory) bool {
	switch category {
	case EventForced, EventState, EventAnomaly, EventHeartbeat:
		return true
	default:
		return false
	}
}

func observationTime(at time.Time) (time.Time, error) {
	if at.IsZero() {
		return time.Time{}, errors.New("qualification observation time is zero")
	}
	return at.UTC(), nil
}

func timePointer(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

// confidence: high
