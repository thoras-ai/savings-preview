import os
import re
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scripts"))
import build_report


class BuildReportTest(unittest.TestCase):
    def test_page_is_self_contained(self):
        html = build_report.build()
        self.assertNotIn("{{", html)
        self.assertEqual(re.findall(r"""(?:src|href)=["']https?:""", html), [])
        self.assertNotIn("@import", html)
        self.assertIn("data:font/woff2;base64,", html)


if __name__ == "__main__":
    unittest.main()
