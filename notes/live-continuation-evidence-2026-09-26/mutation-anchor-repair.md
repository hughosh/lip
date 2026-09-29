# Mutation anchor repair

The existing quick-gate receipt `loop/gates-out/20260927T003756222496Z-f22e000e/receipt.json` failed the catalogue anchor test because `M-6W5-NODEPLOYPREFLIGHT` and `M-3AF-NOLOCK` each matched zero times. Its `catalogue-anchors.log` records both failures.

Updated only those two entries in `scripts/harness_negative_control.py`. The deployment mutation now matches the current `return err` preflight and still removes the entire alert validation block. The lock mutation replaces only `lifecycle.AcquireInstanceLock` with a typed nil lock and nil error, leaving the refusal branch and its `syscall` use in place.

After the edit, `/Users/hugh/kek/.venv/bin/python scripts/test_harness_negative_control.py TestRealCatalogueAnchors` passed (2 tests). `git diff --check -- scripts/harness_negative_control.py` also passed. No Go gate or mutation was run; these remain for the serialized integration run.
