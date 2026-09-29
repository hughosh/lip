# Session change and preservation inventory

Starting manifest timestamp: 2026-09-26T20:29:14.852208+00:00.
Inventory refreshed: 2026-09-26T22:20:01+00:00.

Compared 340 starting-manifest paths in scope: 293 unchanged, 47 changed, 0 missing. The retained manifest contains 429 paths total; exclusions are documented in the JSON evidence.

Changed starting paths by subsystem: go: 39, loop: 2, notes: 2, root: 1, scripts: 3.

Changed starting paths:
- `config.example.md` — starting SHA-256 `eac3f7f502eac48f9e4526a9939f17cdca08f7a458184aec3cd6337fd68363f1`, current SHA-256 `bb500381f2177eeeaac668430a3a7c464c64df6402392916b0fecf5581206e8c`.
- `go/cmd/harness/alerts.go` — starting SHA-256 `ca3a2cf01cdd5a98407ef23cff28464f23af21a44b50036f4cf21ee8c44d382a`, current SHA-256 `e0ba78faa49f75e661c3ae91304cc503cf640a66115458a77eaa91204e35ee97`.
- `go/cmd/harness/alerts_test.go` — starting SHA-256 `5508a169834ed0e4d05324e7c494750452e5c85b3dba2c581da9f0c0a3aa9629`, current SHA-256 `42e258a988b11f7684e2ba3a386e862d78d61739d35f97a2dddc04da01b960d9`.
- `go/cmd/harness/config.go` — starting SHA-256 `e664726e249e4799304f6a2b1af3965e8840340ffbb079eadc6842c134f93d92`, current SHA-256 `4211097bf9c48d72804435ee459bf730d0a6f213d4029cedce573e2d048a4ce7`.
- `go/cmd/harness/dispatch.go` — starting SHA-256 `44edf8ab8f9043073a7b2a86bcda3cfb17dba09def39f2240b9e2f9e5ed48ce8`, current SHA-256 `fbaed994bf4a27a6f455494057ec90d86c2c62526a63b8e749be8fd008cb033c`.
- `go/cmd/harness/main.go` — starting SHA-256 `1c9dc45166723998f518e9f9a9f94fafff0b07128203d534a434be115c3bdbcd`, current SHA-256 `3623a7d4cc072dc12dd218af7636be978d4f2beed1145f55ecc48eca1f0ddec1`.
- `go/cmd/harness/provision.go` — starting SHA-256 `46b6e7fa848e254600bc14dbbee1188ffe8ce6f6496c0de376d44e7683cde2a6`, current SHA-256 `9c0f7cf5d541e7a2765b8d3cba5eacdb9ce45817dc115e902a3d889081f0fa84`.
- `go/cmd/harness/provision_test.go` — starting SHA-256 `9cb33441ee404860d364408eed2261371c92b463227a2349ca924d33dea45630`, current SHA-256 `0df0831c82af5b3560caf3266c7eae68ead9971348e589ecf2e3badd4c9432b1`.
- `go/cmd/harness/run.go` — starting SHA-256 `8f7aaad2438a4b45f6199760807f7f2911fdad4826e69ad96a5acab315763f3a`, current SHA-256 `9d8a9a1ac55653bb77a172f33cdc98a9431241ac5bdf07201c65493148a6e783`.
- `go/cmd/harness/runtime.go` — starting SHA-256 `128de851e3c985d633c3e9c6cd97abc807a580ac745d35b3f096649495ff014a`, current SHA-256 `d622c162f1a3406d3063e48625c9366056e32343ab7664b98280cf00a9e76795`.
- `go/cmd/harness/seam_test.go` — starting SHA-256 `fc182e87bf663f72f18b73f329e878eb7b357b4383d3945e3ca4c4e70fe0bbbb`, current SHA-256 `53c43b06e392d69c2d849b1a205a7aa8adc067fa62263603ff014cdd15ee42a1`.
- `go/cmd/harness/shutdown.go` — starting SHA-256 `b0281214935dc7f7eed7be3b122dd1f2724e9eb7d890e49596854d93b76ccceb`, current SHA-256 `fa43b90c17661929adb2cfdecef7bee66e58c4dffc056ef5deb4e546f591f5b1`.
- `go/cmd/harness/shutdown_test.go` — starting SHA-256 `e8cb6185e8b8bd4e91e06ea8e3983d9fc6a69013990affc47be41d468480eb7e`, current SHA-256 `b001ad51259f14b532979fecf15c24c192c5b44ae32f65c2b34177832b693fa1`.
- `go/harness/cfg/params.go` — starting SHA-256 `0772c68852195c3ee2135847dc0438ae05d876d1a29414e46e975b24935d0c86`, current SHA-256 `e2decb3e015eb6d9c6ef7c859fc1a7ca5671240f89dddedd6d9610b9dc648a08`.
- `go/harness/hstore/records.go` — starting SHA-256 `ca66d35116fb093169e14758ec28f82bb144121690c5c8f9e2943547f8177e4d`, current SHA-256 `99a388125ffb008fe0c8ffc914972aa0781bbcdb64ed0b8b0e1b7b54a57d2ead`.
- `go/harness/hstore/schema.go` — starting SHA-256 `d057c95d9aa5d695908b8fd387f1ea77aa68121dc6a2bc947724158cdfaad2cc`, current SHA-256 `2ee6881a12e750aef6597d820af36c8aac392b078b942a5d0ab7e79a86304254`.
- `go/harness/hstore/schema_test.go` — starting SHA-256 `da952f5d35050b3a1d355bd9ea8f7e921c37bca507e4973de1a469b9f48d6d19`, current SHA-256 `965c6b606bda174b48e18c228f3ed2e7eea7e247633a468c8a42f404a2a24070`.
- `go/harness/hstore/seal_test.go` — starting SHA-256 `201ec7faa46e3ecbbad9fd4604f8fb77aa6231efbc8dcb0d0c92e48bc22cafa1`, current SHA-256 `c1c1045593ed99b465cb3af78a0dca3807f5d7181182cfa73f99a12f77c736a4`.
- `go/harness/hstore/sqlite.go` — starting SHA-256 `f14e41985deb656249c14598695949ff63d3c95d236c0484477af66248662fcf`, current SHA-256 `d55441687e2b40db69cde3c57c309bcc6dd6df85095d99c19ac528d1b4e7d830`.
- `go/harness/hstore/writer.go` — starting SHA-256 `2bb2688da0c35f1b38adad44a957fad7cce8b6d8eb64a360b1198a7a173e3920`, current SHA-256 `c533336d01449adc02e5d9c3f25c4b2cc67231a99cd185c864b0028b17960042`.
- `go/harness/lifecycle/drain.go` — starting SHA-256 `7aaf93e9519709901b812b624fef7c5ed44279b306a8aa5d707e910e04e90353`, current SHA-256 `d9cafa83246fc847b0e2ee6625c84c6dadd77fc8e7b3854cddc7151dc16a4da8`.
- `go/harness/lifecycle/startup.go` — starting SHA-256 `e1dfe4b8d61de050cbabc53119aad3b30b0aa6498d361b89999df391abf54a73`, current SHA-256 `fe7863850f8119bc938fe0661f50935a9116e7f80b52d98f4619f72ad98c7be0`.
- `go/harness/ping/service.go` — starting SHA-256 `9ac26524ab314200974fde0146906a8c6b0c871b9be90d35481220e41b4e1c5a`, current SHA-256 `35d6487aad3f65e95128b5bccc1b2092e6de63d1470b6edbc5a688bc1b8259c7`.
- `go/harness/quote/cross.go` — starting SHA-256 `be1c49b39e7a6923d08bc019d6781820344489b3bb57193494210afeff18f399`, current SHA-256 `a59a512ad51e8bb24a41be2e35d42f94707c96794e2fe229ba15666d8cda83db`.
- `go/harness/quote/queue.go` — starting SHA-256 `cb8ff64e2b4876fea1f4142b7aa1cfb409dfda75f77a0791f1a941a80dc9fe2d`, current SHA-256 `d1aa1afabda70b6fb50e9a0e278df336d3331a6dd42ce42c541b32ae4ca78b36`.
- `go/harness/rest/cancel.go` — starting SHA-256 `ef73c35aa93174d2be2ea505e978e978bec85c87df7a4b32702bef7ea010370b`, current SHA-256 `bfc2f6724806e5f26874c2d07d11a4efe3d3f4afcf50fb365258f9c68f7fef1b`.
- `go/harness/rest/cancel_test.go` — starting SHA-256 `df449c54d024ceea00f53755c038b71ea8d6c65bbd14408d3b1913c575b643e4`, current SHA-256 `574c19ee81f5972c4e5a3de479e914715655418642ba4e6d8b0f6fb21b38b606`.
- `go/harness/rest/client.go` — starting SHA-256 `510b44a9e149a0431ab04b2c9270a5724266d57d2319aadbe30c22cdb1e81c84`, current SHA-256 `466e0c331946aab96f9f40b304dd62f9cd52dcb0659b3a1dec8a139703f80f19`.
- `go/harness/rest/page.go` — starting SHA-256 `1ad6afad4cb3066f30ac826b67e81ff694b2619e2f46468064a286fd62294587`, current SHA-256 `1d3a55e79d4f979821c59387a0533667127e52bfd5c47ca1bdedaba94e1f945a`.
- `go/harness/rest/read.go` — starting SHA-256 `184b49231efde99c53a178afc5388de9c09e235f68113c5e50e86b27332a10d7`, current SHA-256 `b87ea739e1e541c12304d9f657f97622490848f6f201c76ef205d9e5933b6f7d`.
- `go/harness/rest/schedule.go` — starting SHA-256 `6172accc7a178fc9c1ed461e643718180da16855325f1e52717fc0bc1dba431c`, current SHA-256 `70a7e0a6d287cc349a2c9ce8626dd56a85a2e1faca8d4df6d0bec92d89cd3739`.
- `go/harness/rest/wire_test.go` — starting SHA-256 `9042fbda7c00ea0f482a4124a4a93294528135bf6db5e6196fe2c687686a64b4`, current SHA-256 `e3c5df6373b4fc43cd676e17d774eb5bcbdcd885c6e2d177f041e8d39595302a`.
- `go/harness/rest/write.go` — starting SHA-256 `efbfc837eb465789d3fb377db1ca863f042ed1606fb77a3588c4e8f9830723dc`, current SHA-256 `7e203cca2683fddcaf75e5c13a395e5d4fce13741d2d7de0477b89906acf3f9f`.
- `go/harness/rest/write_test.go` — starting SHA-256 `478a1ff0686c1e2db8a4c410f5b775678f8038c0385986e341082683ebd6d70a`, current SHA-256 `796fdde614176119d526d42a37fd89a185df76fa39f66a5e5ad8632cd4785055`.
- `go/harness/risk/snapshot.go` — starting SHA-256 `e5c4d38613e2f38a291156fe76779fbdd035a8e1e91c02c35756478c9fbd6255`, current SHA-256 `7760eba6f6e218c68b9df8ea96535466c7919775177e9089598b283ef37782d8`.
- `go/harness/wsx/binding_test.go` — starting SHA-256 `bf759ff6bfc1cc26fc44e5f8f8e3a77ed08d51a263fec43f1b3c98406d6e1643`, current SHA-256 `282cc46512ed5240d44e53f192be9ecacb8cb5e360adf105adf598acc1a0fc5e`.
- `go/harness/wsx/gate.go` — starting SHA-256 `768731cc0c75767a8f55b4cf1f9bfc35554d2d95819ab2215da4f2cdd601e5fb`, current SHA-256 `9c321cb42c1180085c129fe379fb3e8afcc144c6624bfd782a5da417a4be2739`.
- `go/harness/wsx/gate_test.go` — starting SHA-256 `1114e99495f652469873777f69f1f1ef9f649e2b25878c174ef9e4dee3e9e370`, current SHA-256 `63a22d7a6a4cab22dc6016c0c2e874a84af0488613a928aba8fb84c87ca3c74a`.
- `go/harness/wsx/portfolio.go` — starting SHA-256 `184eb3457aa00ad583de09309f3aeacd79575da942857e472146aba9b2b3f1ec`, current SHA-256 `273d4f5ac575b9f0e9052bc1bf2344190181bc5376687c5d060842d9602e5580`.
- `go/harness/wsx/seal_test.go` — starting SHA-256 `0d2db7b3ada4f1d3b97b18e74154d555036fa18de3931055b13bd6c1c5cb1187`, current SHA-256 `20a31f4f08c1a043402dfc9686a4a83a52fa3c7f10a94c01dc48a8277a556e9e`.
- `loop/protocol/RULES.md` — starting SHA-256 `2d56751c6f41ca964fc71242fd48471378d13081f14468d5945de9afb851a9f2`, current SHA-256 `4063e9a9340ba1bf7d660d4c7b5aeaecf7f274ebe42c67f0637901aa5b56b592`.
- `loop/protocol/implement.md` — starting SHA-256 `c213d22c581bb14086dd9081b9be732b9ed75757f7419daa448b9c18deadedd7`, current SHA-256 `b51248d43581fb8426f2e0d47ac9678a948765280a4d6bdf2c08fc304c94afce`.
- `notes/harness-spec.md` — starting SHA-256 `4b894240a4501f3283bbafb112480e66eff3e4259b5ddba7a9da881ee23d92e6`, current SHA-256 `7d020386404ca15f28fb438fc27acde54fde6a847a2beee7b2e3b9ccfd2266f0`.
- `notes/pilot-plan.md` — starting SHA-256 `f5beb4384ea2547dde4719644a5f3fdb898833d6da6e95c81dbd10ba1337c20f`, current SHA-256 `3eb677e4b7a70205f175f3c4ddffc31d6c811ce2c9bdc5492ce2468a22e4dd47`.
- `scripts/check.py` — starting SHA-256 `c609110426f4e186d34a57dc5081ca12b4c03c088e634d61f745ac7f8fe80fb4`, current SHA-256 `72efb1b1a98e8c060840ceb7d1db35f24f73562e47c74612d821626d21e6e52f`.
- `scripts/harness_negative_control.py` — starting SHA-256 `93c5322c90cf882031dedf245b4168c85a47c2d54bff618c68dd3f037683aef3`, current SHA-256 `92029c3cb5334cefd7077c54185fa811cac17fc5e84260622f84190aa373dc27`.
- `scripts/test_harness_negative_control.py` — starting SHA-256 `d336382db3b48411b81679b45a0296c195917031bd67e81580620228b7547555`, current SHA-256 `8f7f3f16c0c2ff79603d604ab714f09af524732bd1a09524c5f452d970099fab`.

Missing starting paths: 0.

New in-scope paths absent from the starting manifest: 52 (go: 49, notes: 2, scripts: 1).

New `go` paths:
- `go/cmd/harness/balance_telemetry.go`
- `go/cmd/harness/balance_telemetry_test.go`
- `go/cmd/harness/capital_dispatch.go`
- `go/cmd/harness/capital_dispatch_test.go`
- `go/cmd/harness/clock_step_composition_test.go`
- `go/cmd/harness/crosscheck.go`
- `go/cmd/harness/crosscheck_test.go`
- `go/cmd/harness/disconnect_composition_test.go`
- `go/cmd/harness/dispatch_throttle.go`
- `go/cmd/harness/dispatch_throttle_test.go`
- `go/cmd/harness/drift_composition_test.go`
- `go/cmd/harness/exposure_identity.go`
- `go/cmd/harness/foreign_orders.go`
- `go/cmd/harness/foreign_orders_test.go`
- `go/cmd/harness/inventory_hard_test.go`
- `go/cmd/harness/inventory_stuck.go`
- `go/cmd/harness/inventory_stuck_test.go`
- `go/cmd/harness/operations_readiness.go`
- `go/cmd/harness/operations_readiness_test.go`
- `go/cmd/harness/portfolio_stop.go`
- `go/cmd/harness/position_journal_test.go`
- `go/cmd/harness/program_membership.go`
- `go/cmd/harness/program_membership_test.go`
- `go/cmd/harness/read_throttle.go`
- `go/cmd/harness/read_throttle_test.go`
- `go/cmd/harness/reject_response.go`
- `go/cmd/harness/reject_response_test.go`
- `go/cmd/harness/reject_window.go`
- `go/cmd/harness/reject_window_test.go`
- `go/cmd/harness/restart_composition_test.go`
- `go/cmd/harness/scenario_exchange_test.go`
- `go/cmd/harness/signal_composition_test.go`
- `go/cmd/harness/sleep_relay_test.go`
- `go/cmd/harness/unknown_composition_test.go`
- `go/harness/hstore/balance_telemetry.go`
- `go/harness/hstore/balance_telemetry_test.go`
- `go/harness/lifecycle/drain_concurrency_test.go`
- `go/harness/num/side.go`
- `go/harness/ping/clock_step_test.go`
- `go/harness/quote/mixed_cancel_test.go`
- `go/harness/rest/orderbook.go`
- `go/harness/rest/orderbook_test.go`
- `go/harness/rest/throttle.go`
- `go/harness/rest/throttle_test.go`
- `go/harness/wsx/forced_crosscheck.go`
- `go/harness/wsx/forced_crosscheck_test.go`
- `go/harness/wsx/observation_gap.go`
- `go/harness/wsx/observation_gap_test.go`
- `go/harness/wsx/read_throttle.go`

New `notes` paths:
- `notes/cr1-operations-runbook.md`
- `notes/invariant-enforcement-2026-09-26.md`

New `scripts` paths:
- `scripts/test_check.py`

Frozen subsystem SHA comparison:
- `go/core`: 10/10 starting paths unchanged; changed 0, missing 0.
- `go/feed`: 5/5 starting paths unchanged; changed 0, missing 0.
- `go/store`: 3/3 starting paths unchanged; changed 0, missing 0.
- `go/cmd/rig`: 2/2 starting paths unchanged; changed 0, missing 0.

Excludes .beads, evidence directories (any path component containing evidence-), this inventory directory, database files, and credential/secret/token-named files. New generated evidence/gate outputs are excluded by evidence-directory scope; source scripts/tests remain in scope. Subsystem grouping uses the first path component, or root for top-level files.
The manifest covers only listed paths, and SHA equality does not establish semantic or runtime correctness. Differences are relative to the manifest timestamp and do not by themselves establish authorship. No tests or gates were run for this inventory task.
