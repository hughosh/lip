# Readiness evidence — 2026-09-26

Read the [assessment](../readiness-2026-09-26.md) and `verification.json` for the
verdict and limitations. These are offline client checks and public API
documentation findings, not operational qualification or permission to trade.

- `start-manifest.json`, `preservation-check.json`, and `session-diffs/` identify
  the pre-existing work and isolate this session's changes. Reconstructed
  before-images are accepted only when their SHA-256 exactly matches the
  manifest captured before edits.
- `api-review.json` records dated official-document findings and direct URLs.
  It contains no authenticated conformance or account-funding measurement.
- `final/source-freeze.json` fingerprints the tested source, tests, gates,
  example configuration, and frozen specification. `final/mutation-inventory.json`
  names the twelve added semantic controls; the full catalogue has 300 entries.
- `final/` contains final check receipts and separate partial mutation reports.
  Every partial report retains its subset identity; none replaces the historical
  `notes/harness-negative-control.md` or establishes full-catalogue outcomes.
- `intermediate/` deliberately retains failed attempts, including stale test
  fixtures and the first race-run store-drain timeout. Passing a later run does
  not erase these observations.
- `rejected-fill-race/` is a **rejected intermediate implementation experiment**.
  A PASS in its demonstrator means it successfully observed an unwanted create
  from stale local inventory after an exchange-side fill. It is not a safety
  pass. The legacy comparison restores old accounting in that temporary
  candidate; it is not a claim that an exact original-tree qualification was
  run. The final implementation instead waits for fresh portfolio truth.
- `prior-gates-out/` is the gate output already present when this session
  started, preserved before `loop/gates.sh` replaced its working output folder.
  It is historical evidence, not a verdict on this session's final source.

No live orders or positions were opened by this work. No operational evidence
bundle or account-cleanliness claim is fabricated from unit tests.
