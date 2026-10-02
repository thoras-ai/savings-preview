#!/usr/bin/env python3
"""Build the report page into one self-contained HTML file with no external requests.

Usage: build_report.py [OUTDIR]          (default: dist)
       build_report.py --site OUTDIR

Writes OUTDIR/index.html. The CLI serves it next to the contract file, saved as
savings-preview.otlp.json, or passes another name in the `data` query parameter.

With --site the page opens on the upload screen and lists the sample files,
which are copied to OUTDIR/samples/. That is the GitHub Pages build.
"""
import base64
import html
import json
import os
import shutil
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
SCRIPTS = [
    "src/js/model.js",
    "src/js/files.js",
    "src/js/state.js",
    "src/js/prices.js",
    "src/js/levers.js",
    "src/js/components.js",
    "src/js/chart.js",
    "src/js/sections.js",
    "src/js/section-start.js",
    "src/js/section-summary.js",
    "src/js/section-money.js",
    "src/js/section-workloads.js",
    "src/js/section-cluster.js",
    "src/js/section-assumptions.js",
    "src/js/section-method.js",
    "src/js/print.js",
    "src/js/app.js",
]
STYLES = ["design/zinc.css", "design/primitives.css", "design/semantic.css", "design/elevation.css", "src/styles/report.css"]
WORDMARK = "design/assets/thoras-wordmark.svg"
FONTS = [
    ("Roboto", 400, "roboto-latin-400-normal.woff2"),
    ("Roboto", 500, "roboto-latin-500-normal.woff2"),
    ("Roboto", 700, "roboto-latin-700-normal.woff2"),
    ("Roboto Mono", 400, "roboto-mono-latin-400-normal.woff2"),
    ("Roboto Mono", 500, "roboto-mono-latin-500-normal.woff2"),
]
SAMPLES = [
    {
        "id": "demo",
        "file": "fixtures/demo-cluster.compact.otlp.json",
        "name": "Demo cluster",
        "detail": "60 workloads in 8 namespaces, 14 days at 5-minute steps, generated",
    },
    {
        "id": "synthetic",
        "file": "fixtures/savings-preview.otlp.json",
        "name": "Synthetic cluster",
        "detail": "14 days at 5-minute steps, generated",
    },
    {
        "id": "minikube",
        "file": "fixtures/minikube-poll.otlp.json",
        "name": "Minikube, polled",
        "detail": "A short poll of a local cluster, names scrubbed",
    },
]


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        return f.read()


def font_faces():
    out = []
    for family, weight, name in FONTS:
        with open(os.path.join(ROOT, "src", "fonts", name), "rb") as f:
            data = base64.b64encode(f.read()).decode("ascii")
        out.append(
            "@font-face{font-family:'%s';font-weight:%d;font-style:normal;font-display:swap;"
            "src:url(data:font/woff2;base64,%s) format('woff2')}" % (family, weight, data)
        )
    return "\n".join(out)


def build():
    css = "\n".join(read(n) for n in STYLES).replace("/*{{FONTS}}*/", font_faces())
    js = "\n".join(read(n) for n in SCRIPTS)
    if "</script" in js.lower():
        raise SystemExit("script text contains a closing script tag")
    page = read("src/index.html").replace("<!--{{WORDMARK}}-->", read(WORDMARK).strip(), 1)
    return page.replace("/*{{CSS}}*/", css, 1).replace("/*{{JS}}*/", js, 1)


def build_site(out):
    listed = []
    os.makedirs(os.path.join(out, "samples"), exist_ok=True)
    for s in SAMPLES:
        name = os.path.basename(s["file"])
        if not os.path.isfile(os.path.join(ROOT, s["file"])):
            raise SystemExit("%s is missing; generate it with go run ./scripts/gen_demo" % s["file"])
        shutil.copyfile(os.path.join(ROOT, s["file"]), os.path.join(out, "samples", name))
        listed.append({"id": s["id"], "url": "samples/" + name, "name": s["name"], "detail": s["detail"]})
    attr = html.escape(json.dumps(listed), quote=True)
    page = build().replace("<body>", '<body data-samples="%s">' % attr, 1)
    open(os.path.join(out, ".nojekyll"), "w").close()
    return page


def main(argv):
    args = argv[1:]
    site = "--site" in args
    args = [a for a in args if a != "--site"]
    out = args[0] if args else os.path.join(ROOT, "dist")
    os.makedirs(out, exist_ok=True)
    page = build_site(out) if site else build()
    path = os.path.join(out, "index.html")
    with open(path, "w", encoding="utf-8") as f:
        f.write(page)
    print("wrote", path)


if __name__ == "__main__":
    main(sys.argv)
