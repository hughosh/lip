# Alert startup classification repair

The existing gate receipt `loop/gates-out/20260927T004319019583Z-90779e5c/receipt.json` failed only `TestRunAndDeployValidateAlertsBeforeCredentialsOrAccountAccess/{run,deploy}`: the test expected a terminal refusal for an absent env file, while the startup path returned a retryable file-read error. I reproduced that failure before editing with the affected tests.

`productionAlertFactory` now preserves wrapped `*os.PathError` failures as operational errors, allowing supervised launchd to retry an absent or unreadable env file. Errors from a readable env file with an absent, duplicate, or malformed alert destination become `*refusal`, stopping supervised launchd (status 0) and returning status 2 manually. Both run and deploy still load alert destinations before credentials, store access, or exchange requests. Runtime alert-service initialization errors remain operational failures.

Focused verification after the change passed:

```
cd go
go test ./cmd/harness -run 'TestRunAndDeployValidateAlertsBeforeCredentialsOrAccountAccess|TestRunAndDeployRefuseMalformedAlertDestinationsBeforeCredentialsOrAccountAccess|TestAlertDestinationEntrypointExitPolicy|TestStartupRigClassifiesAlertIOAndReleasesLock' -count=1
```

The tests cover missing env-file retry; semantic topic and dead-man failures in run and deploy; manual and supervised exit status for missing env versus malformed topic; and operational alert initialization with lock release. `git diff --check` passed on the edited Go files. No broad gate or live qualification was run in this bounded repair.
