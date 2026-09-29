# Offline unusable-store recovery plan drill

`scripts/ops_recovery.py` still performs its file-only preflight. With
`--snapshot`, `--ownership`, and optional `--final-snapshot`, it additionally
validates an **offline plan**. These JSON files must be retained copies from an
independent, read-only account route and an actual ownership source. Their
`source_ref`, `independent`, `scope`, and `complete` values are input assertions;
the script cannot authenticate the route or prove completeness.

Run with absolute paths for the existing `--db`, `--journal`, and `--latch`
arguments, plus absolute paths for the JSON inputs. The current and final
snapshots use this shape:

```json
{
  "scope": "account_wide",
  "independent": true,
  "source_ref": "retained read-only account receipt reference",
  "observed_at": "2026-09-27T01:00:00Z",
  "complete": {"open_orders": true, "fills": true, "held_positions": true},
  "open_orders": [{"coid": "...", "order_id": "...", "ticker": "...", "side": "yes"}],
  "fills": [{"coid": "...", "order_id": "...", "ticker": "...", "side": "yes"}],
  "held_positions": [{"ticker": "...", "side": "yes"}]
}
```

`side` is exactly `yes` or `no`. The position list contains every currently
held ticker/side pair; an empty complete list asserts flat positions. The
retained ownership input is:

```json
{
  "source_ref": "retained ownership ledger receipt reference",
  "owned_orders": [{"coid": "...", "order_id": "...", "ticker": "...", "side": "yes"}]
}
```

Each owned identity must match all four fields of an order or fill in the
current account snapshot. Missing fields, contradictions, unmatched ownership,
incomplete scopes, and a final snapshot that is not later are rejected. Prefixes
never establish ownership. Open orders without an exact retained match are
excluded, including plausible foreign orders. A held position with a matching
owned fill creates a reduction *obligation* for attended review; it supplies no
size, price, or executable instruction. A held position lacking that evidence
stays unresolved. Mixed owned and foreign fills on the same ticker/side also
require attribution review before action.

The synthetic fixture test uses a corrupt store, one exactly owned order, one
similar-looking foreign order, and a held position. It validates the owned-only
cancellation identity and held-position obligation, then supplies a later
complete flat report. The report is marked snapshot evidence only. The plan
remains `OFFLINE_PLAN_VALIDATED_REAL_RECOVERY_BLOCKED`; it is no cancellation,
reduction, human acknowledgment, flatness attestation, or completed recovery.
See [focused test log](ops-recovery-plan-tests.txt).

Actual clean exit still requires a stopped writer and retained originals,
restored ownership ledger reconciled to independent account truth, owned-only
cleanup receipts, later complete flat truth, human alarm disposition, and a
separate restart decision. No real account or store was accessed in this drill.
