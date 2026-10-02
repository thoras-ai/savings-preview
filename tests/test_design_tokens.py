import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
DESIGN = os.path.join(ROOT, "design")
sys.path.insert(0, os.path.join(ROOT, "scripts"))
import sync_design_tokens

# Tailwind 4's shadow sizes, which the elevation tokens are built from.
SHADOWS = """
  --shadow-xs: 0 1px 2px 0 rgb(0 0 0 / 0.05);
  --shadow-sm: 0 1px 3px 0 rgb(0 0 0 / 0.1), 0 1px 2px -1px rgb(0 0 0 / 0.1);
  --shadow-md: 0 4px 6px -1px rgb(0 0 0 / 0.1), 0 2px 4px -2px rgb(0 0 0 / 0.1);
  --shadow-lg: 0 10px 15px -3px rgb(0 0 0 / 0.1), 0 4px 6px -4px rgb(0 0 0 / 0.1);
  --shadow-xl: 0 20px 25px -5px rgb(0 0 0 / 0.1), 0 8px 10px -6px rgb(0 0 0 / 0.1);
"""


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


class DesignTokensTest(unittest.TestCase):
    def test_tokens_json_matches_the_css(self):
        self.assertEqual(read(os.path.join(DESIGN, "tokens.json")), sync_design_tokens.tokens(DESIGN))

    def test_sync_reproduces_design_folder(self):
        with tempfile.TemporaryDirectory() as tmp:
            dash = os.path.join(tmp, "dashboard")
            tokens = os.path.join(dash, "src", "styles", "tokens")
            os.makedirs(tokens)
            os.makedirs(os.path.join(dash, "src", "assets"))
            for name in ("primitives.css", "semantic.css"):
                shutil.copyfile(os.path.join(DESIGN, name), os.path.join(tokens, name))
            shutil.copyfile(os.path.join(DESIGN, "assets", "thoras-wordmark.svg"), os.path.join(dash, "src", "assets", "logo.svg"))
            zinc = read(os.path.join(DESIGN, "zinc.css")).replace(":root {", "").replace("}", "")
            theme = os.path.join(tmp, "theme.css")
            with open(theme, "w") as f:
                f.write("@theme default {\n" + zinc + SHADOWS + "  @keyframes spin { to { transform: rotate(360deg); } }\n}\n")
            out = os.path.join(tmp, "out")
            subprocess.run([sys.executable, os.path.join(ROOT, "scripts", "sync_design_tokens.py"), dash, "--tailwind", theme, "--out", out],
                           check=True, capture_output=True)
            for name in ("primitives.css", "semantic.css", "zinc.css", "elevation.css", "tokens.json", "assets/thoras-wordmark.svg"):
                self.assertEqual(read(os.path.join(out, name)), read(os.path.join(DESIGN, name)), name)

    def test_page_uses_only_defined_tokens(self):
        doc = json.loads(read(os.path.join(DESIGN, "tokens.json")))
        names = set(doc["primitive"]) | set(doc["zinc"]) | set(doc["elevation"])
        for theme in doc["semantic"].values():
            names |= set(theme)
        import re
        used = set(re.findall(r"var\(--([a-z0-9-]+)", read(os.path.join(ROOT, "src", "styles", "report.css"))))
        self.assertEqual(sorted(used - names), [])


if __name__ == "__main__":
    unittest.main()
