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
        return subprocess.run(
            [sys.executable, str(h.HERE / "harness_negative_control.py"), *args],
            capture_output=True, text=True, cwd=str(h.LIP))

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

    def test_normal_mode_preflights_before_any_go_test(self):
        """Ordering, which is the entire value of the preflight.

        A preflight that ran AFTER the baseline suite would still report the
        stale replacement -- a minute late in the cheap case and ninety-five
        minutes late in the expensive one. So the assertion is not "it detects
        the defect" but "no Go test had run when it did".

        Driven in-process against a patched catalogue, because proving this
        needs a mutation that genuinely does not compile, and such an entry must
        never exist in the real `MUTATIONS`.
        """
        calls: list[list[str]] = []
        real_run = h.run

        def recording_run(cmd, cwd):
            calls.append(cmd)
            return real_run(cmd, cwd)

        stale = ("M-FIXTURE-STALE", "a replacement with obsolete arity",
                 [("harness/lifecycle/startup.go",
                   "\tportfolio := risk.NewSeededPortfolio(pos.ByTicker, s.baseline)\n",
                   "\tportfolio := risk.NewSeededPortfolio(pos.ByTicker)\n")],
                 "TestTheAdoptedPortfolioCarriesTheBaseline")

        argv = ["harness_negative_control.py", "--only", "M-FIXTURE-STALE"]
        with unittest.mock.patch.object(h, "MUTATIONS", [stale]), \
                unittest.mock.patch.object(h, "run", recording_run), \
                unittest.mock.patch.object(sys, "argv", argv):
            rc = h.main()

        self.assertEqual(rc, 1, "a non-compiling replacement must fail the run")
        self.assertFalse(
            [c for c in calls if "test" in c],
            "a Go test ran before the preflight rejected a replacement that "
            "does not compile; the preflight exists to spend seconds on this "
            "rather than the ~105 minutes a full round costs")
        self.assertTrue([c for c in calls if "build" in c],
                        "the preflight did not compile anything")


if __name__ == "__main__":
    unittest.main()
