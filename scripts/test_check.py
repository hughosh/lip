import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import check


class HaltIdentifierTests(unittest.TestCase):
    def test_code_positions_and_lines(self):
        source = """package main
func halt() { halt := 1; _ = halt }
type T struct { halt int }
var _ = T{}.halt
"""
        self.assertEqual(check.halt_identifier_lines(source), [2, 2, 2, 3, 4])

    def test_comments_and_literals_are_ignored(self):
        source = '''// halt
/* halt
   halt */
var _ = "halt \\" halt // halt"
var _ = 'halt'
var _ = `halt
halt /* halt */`
var _ = "/* halt */"
/* "halt" */ var halt = 1 // halt
'''
        self.assertEqual(check.halt_identifier_lines(source), [9])

    def test_identifier_boundaries_follow_go_unicode_rules(self):
        source = """var prehalt, haltSuffix, _halt, halt_ int
var éhalt, halt界, halt７ int
var halt, x = 1, halt
"""
        self.assertEqual(check.halt_identifier_lines(source), [3, 3])

    def test_check_scans_test_files_and_reports_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            go = root / "go"
            go.mkdir()
            (go / "main.go").write_text("package p\nvar halt int\n")
            (go / "main_test.go").write_text('package p\n// halt\nvar _ = "halt"\nvar _ = halt\n')
            with patch.object(check, "LIP", root), patch.object(check, "GO", go):
                self.assertEqual(check.check_halt_identifiers(), [
                    "go/main.go:2:H-HALT-1: forbidden identifier halt",
                    "go/main_test.go:4:H-HALT-1: forbidden identifier halt",
                ])


if __name__ == "__main__":
    unittest.main()
