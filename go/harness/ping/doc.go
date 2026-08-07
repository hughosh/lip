// Package ping is harness-spec.md §13: the operator's only channel.
//
// # What this package is for
//
// An unattended harness that cannot tell anyone anything is a harness nobody is
// running. §13 gives it three obligations -- journal every anomaly before
// attempting delivery, rate limit by severity, and check in with an EXTERNAL
// dead man so that the absence of the process is itself detectable (F18) -- and
// this package implements all three against real transports.
//
// # Delivery is durable, and this package owns none of it
//
// Every anomaly row lives in `hstore`, is journalled there, and has its delivery
// state updated through `hstore`'s writer. There is deliberately no SQL here and
// no in-memory pending list that could outlive a row or be outlived by one. That
// is what makes `TestUndeliveredAlertsSurviveRestartAndDrainOldestFirst` a claim
// about the system rather than about a map: the queue that survives a restart is
// the table.
//
// # Step is deterministic
//
// `Service.Step` takes the time as an argument, reads no clock and starts no
// goroutine. `lip-3af` owns the loop and the ticker. That is the same rule the
// rest of the harness is built on, and here it buys the ability to assert a
// fifteen-minute rate limit in a test that runs in a millisecond and never
// sleeps.
//
// # The two secrets
//
// The ntfy topic IS the credential -- anyone holding it can read and write the
// channel -- and so is the dead-man check-in URL. Both are opaque types with no
// `String` method, they are loaded only from an explicit absolute env-file path,
// and neither appears in a log line, a database column, a journal record, a
// notification title, or a notification body. The only place the topic is
// permitted to appear is the destination URL of the request that delivers to it.
//
// # What is NOT here
//
// Provisioning the ntfy topic and the dead-man provider, and PROVING that a
// missed check-in actually fires an alarm, are operational work in `lip-3af`'s
// live-entry gate. §13.4's whole point is that an untested dead man is a dead
// man's switch nobody has established is connected, and no boolean in this
// package can stand in for that evidence.
package ping

// confidence: high
