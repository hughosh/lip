// Package hstore is harness-spec.md §15's operational store: the five pilot
// records, their sole writer, the durable ownership barrier, and the read-only
// views the ping service delivers from.
//
// # Why there is exactly one writer
//
// §15's tables are not a log. Two of them are LICENCES -- `owned_order` decides
// whether a fill is ours (H-ORD-9) and whether an order may be dispatched at all
// (H-ORD-6), and `anomaly` decides what the operator is ever told. A second
// writer would make both racy in the direction that reads "not ours" and "never
// alerted", which are the two answers this repository has already been burned
// by. So there is one connection, one goroutine, and no generic SQL escape
// hatch on the public surface.
//
// # Why submission is asynchronous and audit records are never dropped
//
// Every pilot record is audit-grade. A store that shed load under pressure would
// shed exactly the rows describing the pressure. The queue is therefore an
// unbounded FIFO with a capacity-one wake channel: submission takes a mutex
// briefly and never waits for SQLite, a stalled head stays queued and is
// retried, and nothing is evicted. `M-HS-AUDITDROP` breaks that on purpose.
//
// # H-STORE-3: persistence failure revokes ADDING and nothing else
//
// This is the explicit correction to F19. When the store cannot write, the
// harness loses the authority to add risk -- and keeps reducing it, keeps
// monitoring it, keeps producing audit records, keeps heartbeating, and keeps
// retrying. A store failure that stopped the reducer would convert a disk
// problem into an inventory problem, which is the inversion the whole design is
// built against. There is deliberately no disk fallback: a second, weaker
// durable path is a second thing to be wrong about.
//
// # The dual anomaly journal
//
// §13.1 journals an anomaly synchronously before any delivery is attempted. An
// anomaly here becomes delivery-visible only after its SQLite row commits with
// `journaled_ms=NULL`, its JSONL line is appended and synced, and `journaled_ms`
// is committed. The JSONL exists because a corrupt or unopenable database is
// precisely the condition under which the operator most needs the text, and the
// ordering exists because a row that is visible to delivery before the text is
// durable can be delivered once and then lost.
//
// Storage paths are explicit constructor arguments and NOT `cfg.Params` fields:
// §16 is the parameter table a run row records verbatim, and where the database
// lives is deployment, not policy.
package hstore

// confidence: high
