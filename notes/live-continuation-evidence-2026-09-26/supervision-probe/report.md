# Account-free macOS supervision probe — 2026-09-26

## Result

On this Mac, the proposed launchd policy (`RunAtLoad=true`, `KeepAlive={SuccessfulExit:false}`, `ThrottleInterval=3`) loaded successfully with `/usr/bin/caffeinate -is` in front of a disposable Python child. A child exit 0 stopped after one run; exit 17 restarted after about 3.1 seconds; `launchctl kill SIGTERM` reached the child's SIGTERM handler, and the child stayed alive for about 3.1 seconds before exit 0 stopped the job. `launchctl kill SIGKILL` caused a restart, with launchd recording `last terminating signal = Killed: 9`. This supports the supervisor mechanism, not financial or harness recovery.

## Setup and exact commands

All artifacts and the child are in this directory. No installed LaunchAgent, existing job, collector, credential, network endpoint, account, or harness was used. The disposable child is `child.py`; `run_probe.py` created four unique plist files here, checked each label with `launchctl print` before bootstrap, and used `bootout` on only the matching job in a `finally` block. Each plist had these exact policy values: `RunAtLoad=true`, `KeepAlive={SuccessfulExit:false}`, `ThrottleInterval=3` and the case-specific `ProgramArguments` below. `plutil -lint` and `launchctl bootstrap` returned 0 for all four plists.

Direct status checks used:

```
/usr/bin/caffeinate -is /usr/bin/python3 /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/child.py zero /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/direct-zero.events.jsonl
/usr/bin/caffeinate -is /usr/bin/python3 /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/child.py nonzero /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/direct-nonzero.events.jsonl
```

Their process statuses were exactly 0 and 17, matching the child's recorded statuses. Each loaded job used the same full argv, with the final two arguments as follows:

| Label | Last two `ProgramArguments` after `/usr/bin/caffeinate -is /usr/bin/python3 <absolute child.py>` |
| --- | --- |
| `com.lip.supervision.probe.zero.3a0c5d5bdc` | `zero /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/zero.events.jsonl` |
| `com.lip.supervision.probe.nonzero.3a0c5d5bdc` | `nonzero /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/nonzero.events.jsonl` |
| `com.lip.supervision.probe.term.3a0c5d5bdc` | `term /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/term.events.jsonl` |
| `com.lip.supervision.probe.kill.3a0c5d5bdc` | `kill /Users/hugh/kek/lip/notes/live-continuation-evidence-2026-09-26/supervision-probe/kill.events.jsonl` |

For each row, the actual plist contains the complete argv and was passed to `/bin/launchctl bootstrap gui/501 <absolute case.plist>`. The signal commands were exactly:

```
/bin/launchctl kill SIGTERM gui/501/com.lip.supervision.probe.term.3a0c5d5bdc
/bin/launchctl kill SIGKILL gui/501/com.lip.supervision.probe.kill.3a0c5d5bdc
```

Both signal commands returned status 0.

## Observations

| Case | Child events and PID | launchd observation |
| --- | --- | --- |
| zero | PID 35954 start, exit 0 | `runs = 1`, `state = not running`, `last exit code = 0` after a further 4-second wait |
| nonzero | PID 36021 start/exit 17; PID 36059 start/exit 17, starts 3.066 seconds apart | `runs = 2`, `last exit code = 17`, `state = spawn scheduled` when the bounded observer ended |
| term | PID 36072 start, received `SIGTERM`, exited 0 3.127 seconds after signal | One second after receipt, still `state = running`, `runs = 1`, `pid = 36072`; after exit and another 4-second wait, `runs = 1`, `state = not running`, `last exit code = 0` |
| kill | PID 36184 start, no exit event; PID 36242 start 3.265 seconds later | `runs = 2`, `pid = 36242`, `last terminating signal = Killed: 9` |

The `term` and `kill` launchd PIDs matched the child PIDs, consistent with `caffeinate` replacing itself with the child for this argv. The clean and failed direct invocations establish that child exit status was preserved. SIGKILL cannot run a child handler, so the missing exit event and launchd's terminating-signal record are the available evidence there. The 3-second throttle bounded the observed retry cadence; it did not impose a finite retry count.

## Cleanup and limits

Each `bootout` returned 0. The immediate `launchctl print` after `kill` bootout briefly returned a loaded record; a later final check in `final-cleanup.json` returned 113 (service absent) for all four labels, and `ps` returned 1 (no process) for every observed PID. No probe job or child remained. The complete command outputs and timestamped child events are in `raw-result.json` and `*.events.jsonl`.

This tested a Python child with deliberate zero, nonzero, and signal behavior. It does not prove the harness's real drain conditions, structural-refusal mapping, Go panic behavior, alarm delivery, account flatness, or live readiness. A persistent nonzero failure would continue retrying under this policy until an operator or another mechanism stopped it.
