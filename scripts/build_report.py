#!/usr/bin/env python3
"""Build the report page into one self-contained HTML file with no external requests.

Usage: build_report.py [OUTDIR]   (default: dist)
Writes OUTDIR/index.html. The CLI serves it next to the contract file, saved as
savings-preview.otlp.json, or passes another name in the `data` query parameter.
"""
import base64
import os
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
SRC = os.path.join(ROOT, "src")
SCRIPTS = ["model.js", "chart.js", "app.js"]
STYLES = ["zinc.css", "primitives.css", "semantic.css", "report.css"]
FONTS = [
    ("Roboto", 400, "roboto-latin-400-normal.woff2"),
    ("Roboto", 500, "roboto-latin-500-normal.woff2"),
    ("Roboto", 700, "roboto-latin-700-normal.woff2"),
    ("Roboto Mono", 400, "roboto-mono-latin-400-normal.woff2"),
    ("Roboto Mono", 500, "roboto-mono-latin-500-normal.woff2"),
]


def read(*parts):
    with open(os.path.join(SRC, *parts), encoding="utf-8") as f:
        return f.read()


def font_faces():
    out = []
    for family, weight, name in FONTS:
        with open(os.path.join(SRC, "fonts", name), "rb") as f:
            data = base64.b64encode(f.read()).decode("ascii")
        out.append(
            "@font-face{font-family:'%s';font-weight:%d;font-style:normal;font-display:swap;"
            "src:url(data:font/woff2;base64,%s) format('woff2')}" % (family, weight, data)
        )
    return "\n".join(out)


def build():
    css = "\n".join(read("styles", n) for n in STYLES).replace("/*{{FONTS}}*/", font_faces())
    js = "\n".join(read("js", n) for n in SCRIPTS)
    if "</script" in js.lower():
        raise SystemExit("script text contains a closing script tag")
    return read("index.html").replace("/*{{CSS}}*/", css, 1).replace("/*{{JS}}*/", js, 1)


def main(argv):
    out = argv[1] if len(argv) > 1 else os.path.join(ROOT, "dist")
    os.makedirs(out, exist_ok=True)
    path = os.path.join(out, "index.html")
    with open(path, "w", encoding="utf-8") as f:
        f.write(build())
    print("wrote", path)


if __name__ == "__main__":
    main(sys.argv)
