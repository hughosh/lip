#!/usr/bin/env python3
"""lip-rj5: promote the live conformance corpus into checked-in REST fixtures.

WHY THIS EXISTS. `find go -iname testdata` was EMPTY. Every fixture under
`go/harness/rest` and `go/harness/wsx` was a hand-written Go literal with
synthetic identifiers -- `o1`, `T1`, `position_fp: "3.50"` -- so the decoders
were only ever tested against payloads written by the same person who wrote the
decoder. That is the structural root cause of the `lip-9tr` defect class, not an
isolated slip: nobody hand-writes the record shape they did not know existed.
A six-decimal `fee_cost`, a `volume` incentive programme carrying no
`target_size_fp`, a market whose status is `finalized` -- all three were found in
one read-only sweep and none were reachable from the existing fixtures.

WHAT IT READS. Both artifacts written by `go/cmd/conform` against the real
account on 2026-08-12:

  notes/conform-corpus.json  the ACCEPTED records, exactly as the exchange sent
                             them: 15 fills, 8 orders, 1 market position,
                             1 event position, 3,276 active programmes.
  notes/conform.json         the sweep report, whose `failures` array carries up
                             to 25 raw records PER ENDPOINT that the production
                             decoder refused.

WHAT IT WRITES. `go/harness/testdata/kalshi/`, consumed by `corpus_test.go` in
BOTH `harness/rest` and `harness/wsx`. It sits at `harness/` rather than under
one package because the record and the code that reads its fields are in
DIFFERENT packages: `rest` decodes a fill, and `wsx.convertFills` is what parses
the `fee_cost` on it. `lip-9tr` was in the second, so a corpus only `rest` could
reach would have missed the very defect that motivated recording one.

TWO FIXTURE CLASSES, AND BOTH ARE LOAD-BEARING. `accepted` records must decode;
`refused` records must NOT, for the stated reason. The refused set exists
because the active-programme walk this account sees today is 100% `liquidity`
with a `target_size_fp` on every record, so an acceptance-only fixture set would
rebuild exactly the blind spot this bead exists to remove. The `volume` shape
(22,318 of them on the unfiltered endpoint, none carrying a Target Size) is a
latent boot failure: `Programs` is first-error-wins and `runtime.go` turns its
failure into a refusal to START, so the day the active set includes one, the
harness does not boot. Pinning the refusal pins the reason.

TRIMMING. The 3,276 active programmes are 10 distinct shapes over the four
fields that decide the decoder's verdict; keeping every one of them would be
1.5MB of the same record. Up to `PER_BUCKET` per bucket is kept, in the order
the exchange returned them, so every shape survives and the file stays readable.

REDACTION, and what it deliberately does NOT touch. Opaque identifiers are
replaced with format-preserving synthetic UUIDs derived deterministically from
the original, so equalities inside the corpus survive -- fill `1582884a...`
carries the same value in `fill_id` and `trade_id`, and it still does after
substitution. `user_id` is the one true account identifier and no decoder reads
it, so it becomes a single fixed synthetic value.

Nothing the decoder INTERPRETS is touched: prices, counts, fees, timestamps,
statuses, sides, tickers, types and `exchange_index` are byte-identical to the
wire. In particular `client_order_id` is substituted but stays a plain UUID,
because these orders were NOT placed by this harness and the decoder is supposed
to notice -- `ParseCoid` requires the `lipH` prefix and fixed-width fields, which
no UUID can satisfy, so the substitution cannot accidentally turn a foreign
order into one of ours.

This is not an anonymisation guarantee: the unredacted corpus sits beside it in
notes/, and the market tickers and timestamps are retained verbatim because the
decoders key on them. The goal is narrower and worth stating exactly -- the
directory that travels with the Go code carries no account identifier.

Usage:
    python3.12 scripts/make_rest_testdata.py [--check]

    --check regenerates into memory and diffs against what is on disk, exiting
    non-zero if they differ. That is the mode a gate would run: it proves the
    checked-in fixtures are what this script produces from the recorded corpus,
    so a hand-edit of a "real payload" fixture cannot pass unnoticed.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
CORPUS = LIP / "notes" / "conform-corpus.json"
REPORT = LIP / "notes" / "conform.json"
OUT = LIP / "go" / "harness" / "testdata" / "kalshi"

# Up to this many records per distinct programme shape. Three rather than one so
# a fixture that accidentally depends on a single record's incidental values is
# visible as a failure on two others.
PER_BUCKET = 3

# Bounded because the refusal fixture pins a REASON, not a census. conform kept
# 25 samples; the shapes among them are identical.
REFUSED_MAX = 6

# Substitution is keyed so the mapping is a property of this script rather than
# of a random draw, and re-running it reproduces the checked-in bytes exactly.
SALT = b"lip-rj5/rest-testdata/v1"

# Opaque identifiers: replaced with a synthetic UUID of identical form. Every
# one of these is either unread by the decoders or read only as an opaque key.
OPAQUE_ID_FIELDS = frozenset(
    {"order_id", "fill_id", "trade_id", "client_order_id", "id", "market_id"}
)
# The account identifier. No decoder reads it; it becomes one fixed value rather
# than a per-value substitution, because preserving how many distinct users
# appear is preserving something about the account.
ACCOUNT_FIELDS = frozenset({"user_id"})
FIXED_USER_ID = "00000000-0000-4000-8000-000000000001"


def synth_uuid(real: str) -> str:
    """A deterministic, format-preserving stand-in for one opaque identifier."""
    digest = hashlib.blake2b(real.encode("utf-8"), key=SALT, digest_size=16).hexdigest()
    # Stamped to look like a v4 UUID so anything that validates the FORM of an
    # identifier still sees a well-formed one.
    return (
        f"{digest[0:8]}-{digest[8:12]}-4{digest[13:16]}-"
        f"8{digest[17:20]}-{digest[20:32]}"
    )


def redact(rec: dict) -> dict:
    """Substitute identifiers in one record, leaving every decoded field alone."""
    out = {}
    for key, value in rec.items():
        if key in ACCOUNT_FIELDS:
            out[key] = FIXED_USER_ID
        elif key in OPAQUE_ID_FIELDS and isinstance(value, str) and value:
            out[key] = synth_uuid(value)
        else:
            out[key] = value
    return out


def program_bucket(program: dict) -> tuple:
    """The fields that decide whether, and how, the programme decoder accepts."""
    return (
        program.get("incentive_type"),
        program.get("incentive_description"),
        program.get("discount_factor_bps"),
        program.get("paid_out"),
        program.get("target_size_fp"),
    )


def trim_programs(programs: list) -> list:
    kept, seen = [], {}
    for program in programs:
        bucket = program_bucket(program)
        if seen.get(bucket, 0) >= PER_BUCKET:
            continue
        seen[bucket] = seen.get(bucket, 0) + 1
        kept.append(program)
    return kept


def refused_programs(report: dict) -> list:
    """The raw records the production decoder refused, from the sweep report."""
    out = []
    for failure in report.get("failures", []):
        if failure.get("endpoint") != "programs.unfiltered":
            continue
        record = failure.get("record")
        if isinstance(record, dict):
            out.append(record)
        if len(out) >= REFUSED_MAX:
            break
    return out


def render(records: list) -> str:
    """One record per line-group, sorted keys, trailing newline: diffable."""
    return json.dumps(records, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def build() -> dict[str, str]:
    corpus = json.loads(CORPUS.read_text())
    report = json.loads(REPORT.read_text())

    files = {
        "fills.json": [redact(r) for r in corpus["fills"]],
        "orders.json": [redact(r) for r in corpus["orders"]],
        "market_positions.json": [redact(r) for r in corpus["positions.market"]],
        "event_positions.json": [redact(r) for r in corpus["positions.event"]],
        "incentive_programs.json": [
            redact(r) for r in trim_programs(corpus["programs"])
        ],
        "incentive_programs_refused.json": [
            redact(r) for r in refused_programs(report)
        ],
    }
    for name, records in files.items():
        if not records:
            raise SystemExit(f"{name} would be empty; the corpus is not what this expects")
    rendered = {name: render(records) for name, records in files.items()}
    rendered["README.md"] = readme(files)
    return rendered


def readme(files: dict[str, list]) -> str:
    counts = "\n".join(
        f"  {name:<34} {len(records):>3} record(s)" for name, records in files.items()
    )
    return f"""# Real Kalshi payloads

Captured from the live account on 2026-08-12 by `go/cmd/conform`, a read-only
sweep, and promoted here by `scripts/make_rest_testdata.py`. Do not hand-edit:
run `python3.12 scripts/make_rest_testdata.py --check` to verify these files are
what that script produces from `notes/conform-corpus.json` and `notes/conform.json`.

{counts}

`incentive_programs.json` is a shape-preserving TRIM of the 3,276 active
programmes the account saw: up to {PER_BUCKET} per distinct combination of
incentive type, description, discount factor, paid-out flag and Target Size.

`incentive_programs_refused.json` is the other half, and it is the reason this
directory is not an acceptance-only fixture set. The active walk this account
sees is 100% `liquidity` with a Target Size on every record, so acceptance alone
would rebuild the blind spot these fixtures exist to remove. These records are
real `volume` programmes carrying no `target_size_fp`; the decoder must REFUSE
them, and `Programs` being first-error-wins means the day one appears in the
active set, the harness does not boot.

## What was substituted

`user_id` is replaced by one fixed value. The opaque identifiers
({", ".join(sorted(OPAQUE_ID_FIELDS))}) are replaced by synthetic UUIDs derived
deterministically from the originals, so equal values stay equal -- a fill whose
`fill_id` and `trade_id` matched on the wire still matches here.

Nothing the decoder interprets was touched. Prices, counts, fees, timestamps,
statuses, sides, tickers, types and `exchange_index` are byte-identical to what
the exchange sent, including the six-decimal `fee_cost` that `lip-9tr` was about.
`client_order_id` is substituted but remains a plain UUID, which is load-bearing:
these orders were not placed by this harness, and `rest.ParseCoid` requires the
`lipH` prefix with fixed-width fields, so no UUID can be mistaken for one of ours.

This is not an anonymisation guarantee. Market tickers and timestamps are
retained verbatim because the decoders key on them, and the unredacted corpus
remains in `notes/`. The narrow claim is the useful one: the directory that
travels with the Go code carries no account identifier.
"""


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="diff against the checked-in fixtures instead of writing them",
    )
    args = parser.parse_args()

    rendered = build()
    if args.check:
        stale = []
        for name, want in rendered.items():
            path = OUT / name
            if not path.exists():
                stale.append(f"{name}: missing")
            elif path.read_text() != want:
                stale.append(f"{name}: differs from the corpus it claims to come from")
        if stale:
            print("\n".join(stale), file=sys.stderr)
            print(
                "\nThese are recorded real payloads, not editable fixtures. "
                "Re-run scripts/make_rest_testdata.py, or restore the file.",
                file=sys.stderr,
            )
            return 1
        print(f"{len(rendered)} fixture file(s) match the recorded corpus")
        return 0

    OUT.mkdir(parents=True, exist_ok=True)
    for name, text in rendered.items():
        (OUT / name).write_text(text)
    print(f"wrote {len(rendered)} file(s) to {OUT.relative_to(LIP)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
