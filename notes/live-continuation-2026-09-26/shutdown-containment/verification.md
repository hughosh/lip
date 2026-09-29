# Test-helper shutdown containment, 2026-09-26

Scope: `go/cmd/harness/seam_test.go` helper startup and `seam_exit_test.go` only. The existing working tree had other edits in `seam_test.go`; those were preserved. Production shutdown code was unchanged.

Commands were run from `go/`, with full output retained beside this note:

| Stage | Command | Result | Log |
| --- | --- | --- | --- |
| Before edit | `go test ./cmd/harness -run '^(TestComposedOperatorSignalKeepsServiceUntilFlatUnrestedExit\|TestAFullTickPlacesTheExitAtTheExternalTouch\|TestSIGTERMWithInventoryDoesNotAuthoriseAnExitAndKeepsEscalating)$' -count=1 -v` | PASS | `before.log` |
| Initial regression | `go test ./cmd/harness -run '^TestSeamHarnessPlannedFlatDrainCapturesExit$' -count=1 -v` | FAIL: no exit callback within 15 seconds | `regression.log` |
| Corrected regression | Same command after using the existing read-only flat fixture | PASS | `regression-readonly.log` |
| Lifecycle/race | `go test -race ./cmd/harness -run '^(TestSeamHarnessPlannedFlatDrainCapturesExit\|TestComposedOperatorSignalKeepsServiceUntilFlatUnrestedExit\|TestSIGTERMWithInventoryDoesNotAuthoriseAnExitAndKeepsEscalating\|TestAFullTickPlacesTheExitAtTheExternalTouch)$' -count=1 -v` | PASS | `lifecycle-race.log` |
| Diff check | `git diff --check -- go/cmd/harness/seam_test.go go/cmd/harness/seam_exit_test.go` | PASS | command output was empty |

The first regression used an armed flat fixture, which may rest quotes; flat alone does not meet the drain's unrested condition. Read-only retains a complete account walk while preventing those orders, allowing the planned flat drain to reach the injected exit callback. The captured code is zero, the durable latch says `sigterm`, and helper cleanup returns. The existing composed SIGINT/SIGTERM test remains passing.

This contains `os.Exit` exposure in the seam test helper. It does not identify the unretained exploratory panic or change production exit behavior.
