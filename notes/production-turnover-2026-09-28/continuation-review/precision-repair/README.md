# REST book crosscheck precision repair

Scope: `go/harness/rest/orderbook.go` plus new `go/harness/rest/orderbook_precision_test.go`. `orderbook.go.before` is the preserved prechange source; `orderbook_test.go.before` preserves the existing test file, which was not edited. `orderbook.go.diff` is the source diff, and the new test source is copied here for review.

The attended anomaly recorded YES depth 2, 23c, websocket size `456.00000000000006` versus REST size `456`. This is one `float64` step. The crosscheck now considers adjacent positive finite sizes equal only when their absolute gap is below `0.005` contract; it still rejects a `0.01` difference. The owned-size lower-bound check uses the same tolerance. Invalid or nonpositive sizes reached during the walk fail. Target accumulation and threshold comparison remain exact naive addition, matching frozen `core.Book.Qualifies`.

Focused command from `go/`: `GOCACHE=/private/tmp/lip-precision-go-cache go test ./harness/rest -run '^TestCompareBooks' -count=1`. The new regression failed before the source change (`before-fix-test.log`) on the observed pair, owned-size roundoff, and invalid infinity. After the change, all focused CompareBooks tests passed (`after-fix-test.log`). `gofmt -l` returned no files.

This does not establish live qualification. A drift of more than one representable step remains a mismatch, and the exact Target Size boundary remains governed by `core`'s naive accumulation.

Root review retained the conservative target-depth boundary and clarified its comment. Additional affected check passed on final source: F5 owner integration plus three existing cancel sweep persistence/opposite-side cases, fresh count=1, no credentials inherited and Go network disabled. Receipt: `affected-owner-and-cancel-tests.json`; log adjacent. No broader gate or q01 was repeated. This proves those local cases, not the unknown intermediate exchange responses of the live cancellation incident.
