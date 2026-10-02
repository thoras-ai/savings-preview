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
        # Nothing loads from the network; the only outside link is the demo booking page.
        self.assertEqual(re.findall(r"""(?:src=|<link[^>]*href=)["']https?:""", page), [])
        self.assertEqual(re.findall(r"""<a [^>]*href=["'](https?:[^"']+)""", page), ["https://www.thoras.ai/book-a-demo"])
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
            # Each sample has an id for links like #sample=demo.
            ids = [s["id"] for s in samples]
            self.assertEqual(ids, ["demo", "synthetic", "minikube"])
            self.assertTrue(os.path.exists(os.path.join(out, ".nojekyll")))

    def test_page_includes_every_script(self):
        on_disk = sorted("src/js/" + n for n in os.listdir(os.path.join(build_report.ROOT, "src", "js")) if n.endswith(".js"))
        self.assertEqual(sorted(build_report.SCRIPTS), on_disk)
        # Sections register themselves, so the registry loads before them and the page wiring after.
        order = build_report.SCRIPTS
        sections = [i for i, n in enumerate(order) if os.path.basename(n).startswith("section-")]
        self.assertLess(order.index("src/js/sections.js"), min(sections))
        self.assertEqual(order[-1], "src/js/app.js")


if __name__ == "__main__":
    unittest.main()
