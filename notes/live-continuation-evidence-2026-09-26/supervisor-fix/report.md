# Supervisor exit-policy patch — 2026-09-26

This is a new candidate after the unchanged q01-v3 binary. No LaunchAgent was installed, loaded, signaled, or changed; no account, credential, browser, or q01-v3 process was touched.

The rendered job now uses `RunAtLoad=true`, `KeepAlive={SuccessfulExit:false}`, and `ThrottleInterval=60`. Its argv starts with `-supervised`. The entry point maps structural and parse refusals to status 0 only for that first-position marker; manual refusals remain 2, operational failures remain 1, planned drains remain 0, and help remains 0. The job still invokes `/usr/bin/caffeinate -is` directly and leaves SIGTERM handling to the existing harness drain path.

Verification from `go/`:

- Before editing: `go test ./harness/lifecycle ./cmd/harness -run 'TestLaunchdPlan|TestInstalledJob|TestAgentArgs|TestMain' -count=1` passed.
- Focused final checks: `go test ./cmd/harness ./harness/lifecycle -run 'TestEntrypointExitPolicy|TestPilotDeployCarriesTheConfigAndRungInStableOrder|TestCanaryDeployRendersExactlyWhatTheOperatorAsserted|TestTheDeployedArgv|TestInstalledJob|TestLaunchdPlan' -count=1` passed. The real entry-point child test subsequently passed after adding a previously valid deployment whose config became structurally invalid: `go test ./cmd/harness -run '^TestEntrypointExitPolicy$' -count=1`.
- The full two-package run was attempted once. `harness/lifecycle` passed. `cmd/harness` failed in two tests on literal deployed-argv expectations still omitting `-supervised`; all four affected expected argvs were updated and the focused tests passed afterward. The full package was not repeated.
- `M-L-KEEPALIVE` first attempt was inconclusive because its replacement left a local helper unused and did not build. The corrected mutation changed the emitted value; baseline was green and `TestLaunchdPlanUsesKeepAliveAndCaffeinateIS` caught it. Receipt: `loop/gates-out/negative-control-20260927T002807295439Z/`.
- `git diff --check` passed.

The earlier disposable macOS launchd/caffeinate probe is in `../supervision-probe/report.md`; this patch did not repeat a loaded-job probe. Planned flat drain is covered by existing harness drain tests, not by the new child test. This patch does not qualify a live run or alter the q01-v3 read-only observation. Root integration should run the default integration check on this new candidate; a fresh candidate gate is needed only if this candidate is proposed for an attended trial.
