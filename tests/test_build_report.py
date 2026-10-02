import html
import json
import os
import re
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scripts"))
import build_report


class BuildReportTest(unittest.TestCase):
    def test_page_is_self_contained(self):
        page = build_report.build()
        self.assertNotIn("{{", page)
        self.assertEqual(re.findall(r"""(?:src|href)=["']https?:""", page), [])
        self.assertNotIn("@import", page)
        self.assertIn("data:font/woff2;base64,", page)
        self.assertIn('<span class="wordmark" role="img" aria-label="Thoras"><svg', page)
        self.assertIn("<body>\n", page)

    def test_site_lists_the_samples_it_copies(self):
        with tempfile.TemporaryDirectory() as out:
            build_report.main(["build_report.py", "--site", out])
            with open(os.path.join(out, "index.html"), encoding="utf-8") as f:
                page = f.read()
            m = re.search(r'<body data-samples="([^"]*)">', page)
            self.assertIsNotNone(m)
            samples = json.loads(html.unescape(m.group(1)))
            self.assertEqual(len(samples), len(build_report.SAMPLES))
            for s in samples:
                self.assertTrue(os.path.isfile(os.path.join(out, s["url"])), s["url"])
            self.assertTrue(os.path.exists(os.path.join(out, ".nojekyll")))


if __name__ == "__main__":
    unittest.main()
