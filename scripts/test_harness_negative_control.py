"""The gate on the gate on the gate (lip-xl7).

`harness_negative_control.py` is the gate that proves the Go tests can fail. This
file is the gate that proves the mutation CATALOGUE is still executable -- that
every mutation's replacement text still compiles against the tree it patches.

WHY IT HAS TO COMPILE SOMETHING. The defect being guarded against is invisible to
text inspection. A mutation carries `old` and `new`; the cheap anchor audit
checks that `old` still occurs exactly once, and `old` goes on matching perfectly
while `new` silently goes stale against a changed signature. Nothing short of a
compiler can tell the difference. So these tests build throwaway Go modules with
the real toolchain rather than asserting on strings.

WHY A SYNTHETIC MODULE AND NOT `lip/go`. A test that reproduced the defect inside
the real tree would have to introduce a genuinely broken mutation into
`MUTATIONS` to have something to catch -- and a deliberately non-compiling entry
in the catalogue is precisely what must never exist there, because every other
consumer of that list treats its contents as executable. The fixture below builds
the defect out of two files it owns completely, so the catalogue stays honest and
the test still exercises the real compiler.

Run: `python -m unittest scripts.test_harness_negative_control` from `lip/`.
"""
from __future__ import annotations

import shutil
import json
import os
import subprocess
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import harness_negative_control as h  # noqa: E402


GO_MOD = "module fixture\n\ngo 1.22\n"

# `helper` takes TWO arguments. A mutation replacement that calls it with one is
# exactly the lip-eyq / lip-da6 defect: the anchor still matches, the call does
# not compile.
HELPER_GO = """package main

func helper(a int, b int) int { return a + b }
"""

MAIN_GO = """package main

import "fmt"

func value() int {
\treturn helper(1, 2)
}

func main() { fmt.Println(value()) }
"""

ANCHOR = "\treturn helper(1, 2)\n"
STALE = "\treturn helper(1)\n"          # obsolete arity -- must NOT compile
CURRENT = "\treturn helper(3, 4)\n"     # still valid -- must compile


def go_available() -> bool:
    return Path(h.GO_BIN).exists()


def anchor_failures(mutations, go_src: Path) -> list[tuple[str, str, int]]:
    """Return every catalogue anchor that is absent or ambiguous.

    This is intentionally only a string scan.  Compiling each replacement is
    the preflight's separate job; this check exists so drift in an `old` anchor
    fails catalogue-tests before the expensive negative-control round starts.
    """
    failures: list[tuple[str, str, int]] = []
    sources: dict[str, str] = {}
    for mid, _name, patches, _expected in mutations:
        for rel, old, _new in patches:
            if rel not in sources:
                sources[rel] = (go_src / rel).read_text()
            count = sources[rel].count(old)
            if count != 1:
                failures.append((mid, rel, count))
    return failures


class TestRealCatalogueAnchors(unittest.TestCase):
    """The cheap, real-tree half of catalogue admission (lip-70u)."""

    def test_every_real_old_anchor_occurs_exactly_once_in_real_tree(self):
        failures = anchor_failures(h.MUTATIONS, h.GO_SRC)
        detail = "\n".join(
            f"{mid}: anchor appears {count}x in {rel}, need exactly 1"
            for mid, rel, count in failures
        )
        self.assertEqual(failures, [], detail)

    def test_rejectlive_style_neighbour_insertion_is_caught_without_go(self):
        """Reproduce the lip-gp8/M-W-REJECTLIVE anchor rot cheaply.

        M-W-REJECTLIVE used to include the function's closing brace in its
        anchor.  Adding mark retirement between `snapGen = 0` and that brace
        left the mutation's meaning untouched but made the old anchor occur
        zero times.  The catalogue test must catch that without compiling or
        running any Go code.
        """
        stale_anchor = ("\tm.quarantined = true\n"
                        "\tm.snapGen = 0\n"
                        "}\n")
        pristine = ("package wsx\n\n"
                    "func quarantineRejectedBook(m *marketGate) {\n"
                    + stale_anchor)
        after_neighbour_change = pristine.replace(
            "\tm.snapGen = 0\n}\n",
            "\tm.snapGen = 0\n\tm.pnlMarkGen = 0\n}\n",
        )
        mutation = (
            "M-W-REJECTLIVE",
            "fixture for an old anchor rotted by a neighbouring insertion",
            [("harness/wsx/gate.go", stale_anchor, "}\n")],
            "TestRejectedBookFrameImmediatelyQuarantinesAnActionableMarket",
        )

        with tempfile.TemporaryDirectory() as td:
            go_src = Path(td)
            gate = go_src / "harness" / "wsx" / "gate.go"
            gate.parent.mkdir(parents=True)
            gate.write_text(after_neighbour_change)
            with unittest.mock.patch.object(
                    h, "run", side_effect=AssertionError("Go must not run")):
                failures = anchor_failures([mutation], go_src)

        self.assertEqual(
            failures,
            [("M-W-REJECTLIVE", "harness/wsx/gate.go", 0)],
        )


class TestJSONVerdicts(unittest.TestCase):
    def stream(self, *events):
        return "\n".join(json.dumps({"Package": "fixture/p", **event})
                         for event in events) + "\n"

    def test_named_failure_is_a_catch(self):
        output = self.stream({"Action": "run", "Test": "TestCatch"},
                             {"Action": "fail", "Test": "TestCatch"},
                             {"Action": "fail"})
        self.assertEqual(h.classify_test_json(1, output, "TestCatch")[0], "CAUGHT")

    def test_additional_failure_does_not_credit_mutation(self):
        output = self.stream({"Action": "run", "Test": "TestCatch"},
                             {"Action": "fail", "Test": "TestCatch"},
                             {"Action": "run", "Test": "TestAlsoCatch"},
                             {"Action": "fail", "Test": "TestAlsoCatch"},
                             {"Action": "fail"})
        verdict, names, _ = h.classify_test_json(1, output, "TestCatch")
        self.assertEqual(verdict, "INCONCLUSIVE")
        self.assertEqual(names, ["TestAlsoCatch", "TestCatch"])
        self.assertEqual(h.classify_test_json(-9, output, "TestCatch")[0], "INCONCLUSIVE")

    def test_other_package_flake_does_not_credit_mutation(self):
        output = (self.stream({"Action": "run", "Test": "TestCatch"},
                              {"Action": "fail", "Test": "TestCatch"},
                              {"Action": "fail"}) +
                  "\n".join(json.dumps({"Package": "fixture/q", **event})
                            for event in ({"Action": "run", "Test": "TestFlake"},
                                          {"Action": "fail", "Test": "TestFlake"},
                                          {"Action": "fail"})) + "\n")
        verdict, names, reason = h.classify_test_json(1, output, "TestCatch")
        self.assertEqual(verdict, "INCONCLUSIVE")
        self.assertEqual(names, ["TestCatch", "TestFlake"])
        self.assertIn("additional test", reason)

    def test_missing_or_unfinished_catcher_is_not_survival(self):
        for events in [
            ({"Action": "run", "Test": "TestOther"},
             {"Action": "pass", "Test": "TestOther"}, {"Action": "pass"}),
            ({"Action": "run", "Test": "TestCatch"}, {"Action": "pass"}),
            ({"Action": "run", "Test": "TestCatch"},
             {"Action": "skip", "Test": "TestCatch"}, {"Action": "pass"})]:
            self.assertEqual(h.classify_test_json(0, self.stream(*events), "TestCatch")[0],
                             "INCONCLUSIVE")

    def test_unrelated_failure_cannot_credit_mutation(self):
        output = self.stream({"Action": "run", "Test": "TestOther"},
                             {"Action": "fail", "Test": "TestOther"},
                             {"Action": "fail"})
        self.assertEqual(h.classify_test_json(1, output, "TestCatch")[0],
                         "INCONCLUSIVE")

    def test_empty_green_is_not_baseline_evidence(self):
        self.assertEqual(h.classify_test_json(0, self.stream({"Action": "pass"}))[0],
                         "INCONCLUSIVE")

    def test_completed_package_without_tests_is_permitted(self):
        output = (self.stream({"Action": "run", "Test": "TestCatch"},
                              {"Action": "pass", "Test": "TestCatch"},
                              {"Action": "pass"}) +
                  json.dumps({"Action": "skip", "Package": "fixture/empty"}) + "\n")
        self.assertEqual(h.classify_test_json(0, output)[0], "GREEN")

    def test_incomplete_or_malformed_runs_are_inconclusive(self):
        output = self.stream({"Action": "run", "Test": "TestCatch"},
                             {"Action": "fail", "Test": "TestCatch"})
        self.assertEqual(h.classify_test_json(1, output, "TestCatch")[0],
                         "INCONCLUSIVE")
        self.assertEqual(h.classify_test_json(1, output + "garbage", "TestCatch")[0],
                         "INCONCLUSIVE")

    def test_timeout_and_panic_are_inconclusive(self):
        output = self.stream({"Action": "run", "Test": "TestCatch"},
                             {"Action": "output", "Output": "panic: broken"},
                             {"Action": "fail", "Test": "TestCatch"},
                             {"Action": "fail"})
        self.assertEqual(h.classify_test_json(1, output, "TestCatch")[0],
                         "INCONCLUSIVE")
        self.assertEqual(h.classify_test_json(124, h.TIMEOUT_MARKER, "TestCatch")[0],
                         "INCONCLUSIVE")


class PreflightFixture(unittest.TestCase):
    """A throwaway Go module plus a catalogue that patches it."""

    def setUp(self) -> None:
        if not go_available():
            self.fail(f"the Go toolchain is missing at {h.GO_BIN}; this suite "
                      f"cannot assert anything about compilation without it")
        self.td = tempfile.TemporaryDirectory()
        self.addCleanup(self.td.cleanup)
        self.lip = Path(self.td.name)
        self.go_src = self.lip / "go"
        self.go_src.mkdir()
        (self.go_src / "go.mod").write_text(GO_MOD)
        (self.go_src / "helper.go").write_text(HELPER_GO)
        (self.go_src / "main.go").write_text(MAIN_GO)

    def mutation(self, mid: str, new: str):
        return (mid, f"{mid} description", [("main.go", ANCHOR, new)],
                "TestSomething")

    def preflight(self, *muts):
        return h.preflight(list(muts), go_src=self.go_src, lip=self.lip,
                           artifacts=())


class TestPreflightDetectsStaleReplacements(PreflightFixture):

    def test_unique_old_with_noncompiling_new_fails_preflight(self):
        """The exact defect: `old` matches once, `new` has obsolete arity.

        This is the case the cheap anchor audit reports as healthy. It must be
        reported here as a failure, or the round spends ~105 minutes to discover
        a defect in the catalogue rather than in the tree.
        """
        src = (self.go_src / "main.go").read_text()
        self.assertEqual(src.count(ANCHOR), 1,
                         "the fixture's own anchor is not unique, so this test "
                         "would not be reproducing the defect it claims to")

        bad = self.preflight(self.mutation("M-STALE", STALE))

        self.assertEqual([mid for mid, _ in bad], ["M-STALE"])
        self.assertIn("helper", bad[0][1],
                      "the report must carry the compiler's own words; an "
                      "operator fixing a stale replacement needs to see which "
                      "symbol moved")

    def test_compiling_replacement_passes_preflight(self):
        self.assertEqual(self.preflight(self.mutation("M-OK", CURRENT)), [])

    def test_preflight_reports_all_noncompiling_replacements(self):
        """Every failure, not just the first.

        One signature change routinely rots several replacements at once, and an
        operator who fixes them one round at a time pays the round each time.
        """
        bad = self.preflight(
            self.mutation("M-STALE-1", STALE),
            self.mutation("M-OK", CURRENT),
            self.mutation("M-STALE-2", "\treturn helper()\n"),
        )
        self.assertEqual(sorted(mid for mid, _ in bad),
                         ["M-STALE-1", "M-STALE-2"])

    def test_a_pristine_tree_that_does_not_build_is_reported_once(self):
        """A broken tree is one finding, not one per mutation."""
        (self.go_src / "helper.go").write_text("package main\n\nfunc helper(")
        bad = self.preflight(self.mutation("M-OK", CURRENT))
        self.assertEqual([mid for mid, _ in bad], ["<baseline>"])


class TestBuildOnlyMode(unittest.TestCase):
    """`--build-only` against the REAL catalogue and the real tree."""

    def setUp(self) -> None:
        if not go_available():
            self.fail(f"the Go toolchain is missing at {h.GO_BIN}")
        self.td = tempfile.TemporaryDirectory()
        self.addCleanup(self.td.cleanup)

    def run_script(self, *args) -> subprocess.CompletedProcess:
        env = os.environ.copy()
        env["GOCACHE"] = str(Path(self.td.name) / "go-cache")
        return subprocess.run(
            [sys.executable, str(h.HERE / "harness_negative_control.py"), *args],
            capture_output=True, text=True, cwd=str(h.LIP), env=env,
            pass_fds=((int(os.environ["LIP_VERIFY_LOCK_FD"]),)
                      if os.environ.get("LIP_VERIFY_LOCK_FD", "").isdigit() else ()))

    def test_build_only_runs_no_go_test_and_writes_no_report(self):
        """It compiles and stops. No report, canonical or partial.

        A file named for the negative control must never be produced by a run
        that executed no test -- the report is the record of what was CAUGHT,
        and a build proves nothing was.
        """
        out = Path(self.td.name) / "should-not-exist.md"
        r = self.run_script("--only", "M11", "--build-only", "--out", str(out))

        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(out.exists(), "--build-only wrote the canonical report")
        self.assertFalse(out.with_suffix(".partial.md").exists(),
                         "--build-only wrote a partial report")
        self.assertNotIn("baseline: green", r.stdout,
                         "--build-only ran the baseline Go test suite")
        self.assertIn("preflight", r.stdout)

    def test_unknown_only_id_is_refused(self):
        r = self.run_script("--only", "M-NO-SUCH-MUTATION", "--build-only")
        self.assertEqual(r.returncode, 2)
        self.assertIn("unknown mutation id", r.stderr)

class TestExecutionCommands(unittest.TestCase):
    """Small synthetic catalogue; no Go process is launched."""

    def setUp(self):
        self.td = tempfile.TemporaryDirectory()
        self.addCleanup(self.td.cleanup)
        self.root = Path(self.td.name)
        self.go = self.root / "go"
        for package, catcher in (("p", "TestCatchP"), ("q", "TestCatchQ")):
            folder = self.go / package
            folder.mkdir(parents=True)
            (folder / "sample_test.go").write_text(
                f"package {package}\nimport \"testing\"\n"
                f"func {catcher}(t *testing.T) {{}}\n")
        (self.go / "source.go").write_text("old\n")
        self.muts = [
            ("M-P", "first", [("source.go", "old", "new-p")], "TestCatchP"),
            ("M-Q", "second", [("source.go", "old", "new-q")], "TestCatchQ"),
            ("M-I", "inert", [("source.go", "old", "new-i")], "inert"),
        ]
        self.calls = []

    def fake_run(self, cmd, cwd):
        self.calls.append(list(cmd))
        if "build" in cmd:
            return 0, ""
        source = (cwd / "source.go").read_text()
        pattern = cmd[cmd.index("-run") + 1] if "-run" in cmd else None
        tests = [("fixture/p", "TestCatchP"), ("fixture/q", "TestCatchQ")]
        if pattern:
            import re
            tests = [(pkg, name) for pkg, name in tests if re.search(pattern, name)]
        events = []
        failed = False
        for pkg, name in tests:
            fail = source == "new-p\n" and name == "TestCatchP" or source == "new-q\n" and name == "TestCatchQ"
            failed |= fail
            events.extend([{"Package": pkg, "Action": "run", "Test": name},
                           {"Package": pkg, "Action": "fail" if fail else "pass", "Test": name},
                           {"Package": pkg, "Action": "fail" if fail else "pass"}])
        return int(failed), "\n".join(json.dumps(e) for e in events) + "\n"

    def execute(self, *args):
        out = self.root / "out" / "report.md"
        out.parent.mkdir(exist_ok=True)
        argv = ["harness_negative_control.py", "--out", str(out), *args]
        with unittest.mock.patch.object(h, "GO_SRC", self.go), \
                unittest.mock.patch.object(h, "LIP", self.root), \
                unittest.mock.patch.object(h, "ROOT_ARTIFACTS", ()), \
                unittest.mock.patch.object(h, "MUTATIONS", self.muts), \
                unittest.mock.patch.object(h, "run", self.fake_run), \
                unittest.mock.patch.object(sys, "argv", argv):
            rc = h.main_locked()
        return rc, out

    def test_missing_and_ambiguous_catcher_fail_inventory(self):
        self.assertEqual(h.catcher_packages(self.muts[:1], self.go),
                         {"TestCatchP": "./p"})
        with self.assertRaisesRegex(ValueError, "missing"):
            h.catcher_packages([("X", "x", [], "TestMissing")], self.go)
        (self.go / "q" / "duplicate_test.go").write_text(
            "package q\nimport \"testing\"\nfunc TestCatchP(t *testing.T) {}\n")
        with self.assertRaisesRegex(ValueError, "ambiguous"):
            h.catcher_packages(self.muts[:1], self.go)

    def test_full_default_builds_once_per_mutant_and_only_inert_runs_full_suite(self):
        rc, out = self.execute()
        self.assertEqual(rc, 0)
        builds = [c for c in self.calls if "build" in c]
        tests = [c for c in self.calls if "test" in c]
        self.assertEqual(len(builds), 3)
        self.assertEqual(len(tests), 4)
        self.assertEqual(sum("-run" not in c for c in tests), 2)
        self.assertEqual(tests[1][-1], "./p")
        self.assertEqual(tests[2][-1], "./q")
        self.assertTrue(out.exists())

    def test_subset_baseline_and_focused_alias_use_named_package(self):
        rc, out = self.execute("--only", "M-P", "--focused")
        self.assertEqual(rc, 0)
        self.assertEqual(len([c for c in self.calls if "build" in c]), 1)
        tests = [c for c in self.calls if "test" in c]
        self.assertEqual(len(tests), 2)
        self.assertTrue(all(c[-1] == "./p" and "-run" in c for c in tests))
        self.assertFalse(out.exists())
        self.assertTrue(out.with_suffix(".partial.md").exists())

    def test_survivor_fails_without_retrying_into_green(self):
        self.muts[0] = ("M-P", "survivor", [("source.go", "old", "still-valid")], "TestCatchP")
        rc, _ = self.execute("--only", "M-P")
        self.assertEqual(rc, 1)
        self.assertEqual(len([c for c in self.calls if "test" in c]), 2)

    def test_compile_failure_never_reaches_catcher(self):
        good_runner = self.fake_run
        def failed_build(cmd, cwd):
            if "build" in cmd:
                self.calls.append(list(cmd))
                return 1, "compiler rejected fixture"
            return good_runner(cmd, cwd)
        self.fake_run = failed_build
        rc, _ = self.execute("--only", "M-P")
        self.assertEqual(rc, 1)
        self.assertEqual(len([c for c in self.calls if "test" in c]), 1)

    def test_suite_runs_full_packages_without_duplicate_builds(self):
        rc, _ = self.execute("--only", "M-P", "--suite")
        self.assertEqual(rc, 0)
        self.assertEqual(len([c for c in self.calls if "build" in c]), 1)
        self.assertEqual(len([c for c in self.calls if "test" in c and "-run" not in c]), 2)

    def test_plan_is_read_only(self):
        import contextlib
        import io
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            rc, _ = self.execute("--only", "M-P", "--plan")
        self.assertEqual(rc, 0)
        plan = json.loads(output.getvalue())
        self.assertEqual((plan["build_runs"], plan["named_test_runs"],
                          plan["full_suite_runs"]), (1, 2, 0))
        self.assertEqual(self.calls, [])
        self.assertFalse((self.root / "out" / "report.md").exists())


if __name__ == "__main__":
    unittest.main()
