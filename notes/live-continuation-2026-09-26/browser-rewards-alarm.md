# Existing-browser read-only inspection

Observed **2026-09-27 00:09:00–00:12:15 UTC** (September 26, 17:09–17:12 PDT).
Reused the two existing logged-in Playwright tabs. No new browser, profile or
session was opened. No financial action, alarm configuration change, ping,
notification or message was sent. Only this redacted evidence note was saved.

## Kalshi Rewards

At 00:10:02 UTC, the visible Portfolio “Rewards (Sep)” popover showed:

- September 2026 rewards: **$0**.
- Lifetime rewards: **$7.65**.
- Visible history: **Liquidity Incentive for event KXGENERICBALLOTVOTEHUB-26JUL31**,
  **July 31, 2026**, **$7.65**, category **Liquidity**.

This is a displayed historical reward entry. The inspection did not obtain an
independent payment/transfer ledger, so it does not establish paid settlement
beyond the UI's displayed credit. There was no visible current personal
provisional earnings amount or estimate-as-of timestamp. Observation timestamps
above are when the UI was read, not provider timestamps for an estimate.

At 00:11:43 UTC, the selected market badge popover displayed:

- Market attribution confirmed at 00:12:15 UTC: **KXTOKENUSE-26SEP28-T146**.
- Shared liquidity rewards pool: **$200**; discount factor **0.5**.
- Period displayed: **September 21, 2026, 9:00 AM PDT** through
  **September 27, 2026, 8:59 PM PDT**.
- Eligible rank described as bids in the top **1,000 shares**.

The $200 is the shared pool, not this account's earned, provisional or paid
reward. No personal earnings estimate or estimate timestamp was exposed in the
inspected market popover. No undocumented endpoint was used or inferred.
Portfolio UI also displayed $98.58 cash, $0 positions and “No open positions”;
that UI snapshot is not complete authenticated account-wide order/position proof.

## Healthchecks.io

The existing monitor **lip harness heartbeat** was **UP** on refresh.
At the first refreshed read (returned 00:09:32 UTC), it showed a last ping
**10 seconds ago**. The refreshed 00:11:43 UTC read showed a last ping
**57 seconds ago**. These relative timestamps are visible UI evidence of recent
pings; this inspection did not send them or identify their producer. An exact
last-ping timestamp and detailed recent event rows were not exposed in the
inspected rendered text/tooltips.

- Period: **1 hour**.
- Grace: **20 minutes**.
- Notification methods: **one enabled email route**, destination redacted.
- September summary: **2 downtimes**, **25 days 16 hours** total, **14.42% uptime**.

The inspected monitor page did not identify a primary versus backup role, show
a separate second enabled route, or establish an acknowledgment-aware escalation
policy. Earlier operator confirmation that email reaches an independent backup
is separate human evidence; it is not proved by this page alone. No fresh alert
receipt, missed-ping firing, recovery drill, or delivery test was performed.

No credentials, cookies, session tokens, email addresses, or opaque monitor
URLs are included in this note. Existing tab navigation/popover state changed
only for reading; trading and alarm settings were untouched.
