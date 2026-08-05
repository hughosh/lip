codex
1. No—not the bot’s adverse-selection cost.

Public data can estimate a market-wide maker markout proxy. For every trade, log:

- timestamp, price, size, aggressor side;
- book immediately before the trade;
- midpoint \(m_{t+h}\) at fixed horizons \(h\);
- eventual settlement \(Y\), if using terminal rather than markout P&L.

For a passive Yes purchase at \(p\):

\[
c_h=p-m_{t+h}, \qquad c_\infty=p-Y
\]

For a passive No purchase at \(n\):

\[
c_h=n-(1-m_{t+h}), \qquad c_\infty=n-(1-Y)
\]

Volume-weight these, preferably stratified by price level, sweep size, spread, depth, volatility, and time-to-expiry.

That estimates toxicity of observed passive executions. It does not estimate your strategy because public data does not identify:

- whether your hypothetical order would have executed;
- its queue position after cancellations and additions;
- which trades would comprise its fill sample;
- the resulting inventory/reposting path;
- actual randomized LIP snapshots.

Short-horizon markouts are also not economic losses unless the future midpoint is an unbiased terminal-value estimate. Settlement markouts are economic, but extremely noisy.

2. A touch trade does not imply your touch order filled.

If \(A\) contracts were already resting when you joined, price-time priority puts you behind them. You fill only after executable volume removes \(A\) contracts ahead of you. The tape shows executions, but aggregate depth updates do not reveal whether cancellations removed orders ahead of you or later orders behind you.

There is one strong conclusion: if an incoming order trades through your price to a worse price, your active order must have filled first. Ordinary prints at your price remain ambiguous.

“Front of queue” assumes the first executable contract at that price hits you. “Back of queue” requires tracking the initial ahead quantity and making an assumption about which queue segment every cancellation removes. Neither is empirically justified by public data.

The bias has no known sign. Front-of-queue produces too many fills, but those extra fills can be either benign or toxic. Back-of-queue selects disproportionately for larger sweeps, which are plausibly more informed, so it may show higher conditional toxicity—but that is not a theorem. Fill probability, conditional markout, inventory, and reward presence all change simultaneously.

3. A sandwich is possible, but likely useless.

Maintain feasible lower and upper bounds on quantity ahead:

- optimistic: cancellations remove ahead quantity first;
- pessimistic: cancellations remove behind quantity first;
- additions after your order remain behind;
- executions consume ahead quantity first.

This brackets fill occurrence. For each feasible fill path, run the complete inventory and P&L policy. Merely comparing average markouts under two fill sets does not guarantee a valid cost bracket because conditional averages are not monotone.

The interval width is driven by same-price depth, cancellation/addition churn, quote lifetime, your size, and frequency of trade-throughs. In a high-churn book, the interval can remain “no fill” versus “many fills” for most touch trades. It becomes narrow only with frequent trade-throughs or very little displayed quantity ahead—exactly the situations most likely to be toxic.

4. No. A single $20 market cannot answer this at reasonable confidence.

Suppose adverse selection is \(\delta\) dollars per contract. Near 50¢, a one-sided terminal position has roughly 50¢ per-contract standard deviation. Contracts in one market are not independent; they share the same settlement outcome. Twenty contracts therefore buy exposure, not twenty observations.

The approximate number of independent market outcomes needed for a 95% signal is:

\[
n\approx\left(\frac{1.96(0.50)}{\delta}\right)^2
\]

That is approximately:

- \(\delta=5\)¢: 384 independent markets;
- \(\delta=2\)¢: 2,401;
- \(\delta=1\)¢: 9,604.

Short-horizon markouts reduce variance, but even with 2–5¢ markout volatility, detecting a 1¢ mean requires roughly 16–96 genuinely independent fill episodes. Fills from one sweep or one drift regime are clustered, not independent.

Paired Yes/No fills remove settlement variance, but then the effective observations are independent completed cycles, not contracts. One probe supplies essentially no defensible power.

5. The framing error is treating “adverse selection net of rewards” as a separable scalar.

The decision variable is expected total P&L of a fully specified quoting policy per dollar of constrained capital and per unit time:

\[
\text{LIP payout}+\text{trading P\&L}-\text{inventory risk}-\text{capital lockup}
\]

Reward capture, repricing, fills, queue position, and adverse selection are endogenous to the same policy. Your probe’s dominant failure was not execution toxicity; it was losing scoring presence after reference-price drift. Chasing the self-referential reference may restore rewards while deliberately increasing toxic fills.

Recommendation: use A only as a rejection/sensitivity tool with explicit queue bounds. B says nothing credible about production economics. Do not use C to decide profitability; it cannot. With $20 and one market, the honest conclusion is “unidentified and radically underpowered,” not “promising but needs more data.”
