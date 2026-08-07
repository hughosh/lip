package hstore

// ---------------------------------------------------------------------------
// §15 — the pilot five, and nothing else
// ---------------------------------------------------------------------------
//
// notes/pilot-plan.md §2.3 cut §15's full table set to five records for the
// pilot. `order_intent`, `order_event`, `position_poll` and the rest are
// DEFERRED, not forgotten, and `TestSchemaIsExactlyThePilotFiveAndPragmasArePinned`
// enumerates `sqlite_schema` to prove the deferred ones are absent. A sixth
// table appearing quietly is how a deferred decision becomes an implemented one
// that nobody argued for.

// schemaVersion is `PRAGMA user_version`. An unknown NON-ZERO version is
// rejected rather than migrated: a database written by a schema we do not know
// is one whose `owned_order` rows we cannot safely read, and reading them wrong
// classifies a fill.
//
// Version 2 added `owned_order.abandoned_ms`. There is no migration and version
// 1 is refused by name, because nothing is deployed: the only version-1 files in
// existence are test fixtures and a developer's scratch database, and a
// migration path written for no user is a code path that is exercised for the
// first time on the day it runs against real evidence.
const schemaVersion = 2

// legacySchemaVersion is the pre-`abandoned_ms` schema. Named so the refusal can
// say WHICH old version it found and why the difference matters, rather than
// falling through to the generic unknown-version message.
const legacySchemaVersion = 1

// The three pinned pragmas. They are constants and not literals inside the DSN
// so that `M-HS-PRAGMA` has exactly one place to corrupt and the test has
// exactly one thing to read back.
//
// `synchronous=NORMAL` under WAL is the documented durable-across-process-crash
// setting; `OFF` is durable across neither a power cut nor an OS crash, and the
// records this store holds are the evidence that a crash happened.
const (
	pragmaJournalMode = "WAL"
	pragmaSynchronous = "NORMAL"
	pragmaForeignKeys = "ON"
)

// userTables is every table this schema is permitted to contain, sorted.
var userTables = []string{
	"anomaly", "our_fill", "owned_order", "run", "state_event",
}

// deferredTables are §15 tables the pilot cut. Named here so the schema test
// asserts their ABSENCE positively rather than by counting -- a count says five
// and does not say WHICH five, and the failure this guards against is a
// deferred decision quietly becoming an implemented one.
var deferredTables = []string{
	"balance_poll", "market", "order_event", "order_intent",
	"position_poll", "snap", "uptime",
}

// Schema is the five pilot records.
//
// Notes on the constraints that are load-bearing rather than tidy:
//
//   - `owned_order.order_id` is UNIQUE and nullable, and the CHECK forces it and
//     `bound_ms` to be null or non-null TOGETHER. A row with an order id and no
//     binding timestamp is a binding whose provenance cannot be reconstructed,
//     and a row with a timestamp and no order id is a binding that classifies
//     nothing. The UNIQUE is also what lets `our_fill.order_id` reference it.
//   - `owned_order.abandoned_ms` is the THIRD state of a reservation, and it is
//     what makes "reserved, dispatched, not yet bound" representable. A row with
//     neither `order_id` nor `abandoned_ms` is a reservation still outstanding:
//     an order may exist on the exchange under that coid whose id we do not know
//     yet, so a fill on an unrecognised order id is not evidence of a third
//     party. The second CHECK forbids a row that is both bound and abandoned --
//     that pair is the ledger asserting the exchange both did and did not take
//     the order, and no reading of it is safe.
//   - `our_fill.order_id` REFERENCES `owned_order(order_id)`. A fill can only be
//     recorded against an order we have durably bound, which is exactly H-ORD-9:
//     ownership is a fact in the ledger, never an inference at the fill.
//   - `state_event` CHECKs a real transition and a non-`none` trigger. A9 needs
//     the reason, and a row that says a market went from QUOTING to QUOTING for
//     no reason is a row that makes the state history unreadable.
//   - `trigger` is quoted because TRIGGER is a SQLite keyword.
//
// `IF NOT EXISTS` throughout: opening an existing database must never recreate
// it, and `Open` never deletes one.
const Schema = `
CREATE TABLE IF NOT EXISTS run (
    run_id      TEXT    PRIMARY KEY,
    started_ms  INTEGER NOT NULL,
    config_json BLOB    NOT NULL
);

CREATE TABLE IF NOT EXISTS owned_order (
    coid        TEXT    PRIMARY KEY,
    run_id      TEXT    NOT NULL REFERENCES run(run_id),
    reserved_ms INTEGER NOT NULL,
    ticker      TEXT    NOT NULL,
    side        TEXT    NOT NULL,
    role        TEXT    NOT NULL,
    price_cents INTEGER NOT NULL,
    count_q     INTEGER NOT NULL,
    order_id    TEXT    UNIQUE,
    bound_ms    INTEGER,
    abandoned_ms INTEGER,
    CHECK ((order_id IS NULL) = (bound_ms IS NULL)),
    CHECK (NOT (order_id IS NOT NULL AND abandoned_ms IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS our_fill (
    trade_id       TEXT    PRIMARY KEY,
    first_run_id   TEXT    NOT NULL REFERENCES run(run_id),
    first_seen_ms  INTEGER NOT NULL,
    backfilled     INTEGER NOT NULL,
    order_id       TEXT    NOT NULL REFERENCES owned_order(order_id),
    ticker         TEXT    NOT NULL,
    side           TEXT    NOT NULL,
    price4         INTEGER NOT NULL,
    count_q        INTEGER NOT NULL,
    fee_micros     INTEGER NOT NULL,
    is_taker       INTEGER NOT NULL,
    exchange_ts_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS state_event (
    event_id   TEXT    PRIMARY KEY,
    run_id     TEXT    NOT NULL REFERENCES run(run_id),
    ts_ms      INTEGER NOT NULL,
    scope      TEXT    NOT NULL,
    ticker     TEXT    NOT NULL,
    from_state TEXT    NOT NULL,
    to_state   TEXT    NOT NULL,
    "trigger"  TEXT    NOT NULL,
    CHECK (scope IN ('global','market')),
    CHECK (from_state <> to_state),
    CHECK ("trigger" <> 'none')
);

CREATE TABLE IF NOT EXISTS anomaly (
    anomaly_id       TEXT    PRIMARY KEY,
    run_id           TEXT    NOT NULL REFERENCES run(run_id),
    class            TEXT    NOT NULL,
    sev              INTEGER NOT NULL,
    ticker           TEXT    NOT NULL,
    text             TEXT    NOT NULL,
    first_ms         INTEGER NOT NULL,
    journaled_ms     INTEGER,
    delivered_ms     INTEGER,
    attempts         INTEGER NOT NULL,
    last_attempt_ms  INTEGER,
    suppressed_count INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_anomaly_pending
    ON anomaly(first_ms) WHERE delivered_ms IS NULL;
`

// confidence: high
