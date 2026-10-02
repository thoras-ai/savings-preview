#!/usr/bin/env python3
"""Copy the dashboard's design tokens into design/.

Usage: sync_design_tokens.py DASHBOARD_DIR [--tailwind THEME_CSS] [--out DIR]

DASHBOARD_DIR is the dashboard folder of the platform repository. The script
copies src/styles/tokens/primitives.css and semantic.css as they are, takes the
zinc scale and the shadow sizes from Tailwind's theme.css (by default
DASHBOARD_DIR/node_modules/tailwindcss/theme.css, so run npm ci there first),
copies the wordmark from src/assets/logo.svg, and writes design/tokens.json
from the CSS it wrote.
"""
import argparse
import json
import os
import re
import shutil
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
DESIGN = os.path.join(ROOT, "design")

# The dashboard's elevation-1 to elevation-5 utilities are Tailwind's shadow-xs
# to shadow-xl, tinted with iris-900 at 20%.
ELEVATIONS = ["xs", "sm", "md", "lg", "xl"]
ELEVATION_TINT = "color-mix(in oklab, var(--primitive-iris-900) 20%, transparent)"
SHADOW_COLOR = re.compile(r"rgb\(0 0 0 / [0-9.]+\)")

SEMANTIC_SELECTORS = {
    ':root, [data-theme="light"]': "light",
    '[data-theme="dark"]': "dark",
    ":root": "fixed",
}


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


def blocks(css):
    """Yield (selector, {name: value}) for each rule, in order."""
    css = re.sub(r"/\*.*?\*/", "", css, flags=re.S)
    for m in re.finditer(r"([^{}]+)\{([^{}]*)\}", css):
        selector = " ".join(m.group(1).split())
        decls = {}
        for part in m.group(2).split(";"):
            if ":" not in part:
                continue
            name, value = part.split(":", 1)
            name = name.strip()
            if name.startswith("--"):
                decls[name[2:]] = " ".join(value.split())
        yield selector, decls


def theme_vars(theme_css, prefix):
    # theme.css nests rules inside @theme, so read the declarations directly.
    css = re.sub(r"/\*.*?\*/", "", theme_css, flags=re.S)
    out = {}
    for m in re.finditer(r"--(%s[\w-]*)\s*:\s*([^;{}]+);" % re.escape(prefix), css):
        out[m.group(1)] = " ".join(m.group(2).split())
    return out


def zinc_css(theme_css):
    zinc = theme_vars(theme_css, "color-zinc-")
    if len(zinc) != 11:
        raise SystemExit("expected 11 zinc steps in the Tailwind theme, found %d" % len(zinc))
    lines = ["  --%s: %s;" % (k, v) for k, v in zinc.items()]
    return ":root {\n" + "\n".join(lines) + "\n}\n"


def elevation_css(theme_css):
    shadows = theme_vars(theme_css, "shadow-")
    lines = []
    for i, size in enumerate(ELEVATIONS, 1):
        value = shadows.get("shadow-" + size)
        if value is None:
            raise SystemExit("the Tailwind theme has no --shadow-" + size)
        lines.append("  --elevation-%d: %s;" % (i, SHADOW_COLOR.sub(ELEVATION_TINT, value)))
    return ":root {\n" + "\n".join(lines) + "\n}\n"


def tokens(design_dir):
    """Build the tokens.json document from the CSS files in design_dir."""
    doc = {"primitive": {}, "zinc": {}, "semantic": {"light": {}, "dark": {}, "fixed": {}}, "elevation": {}}
    for name, key in (("primitives.css", "primitive"), ("zinc.css", "zinc"), ("elevation.css", "elevation")):
        for _, decls in blocks(read(os.path.join(design_dir, name))):
            doc[key].update(decls)
    for selector, decls in blocks(read(os.path.join(design_dir, "semantic.css"))):
        theme = SEMANTIC_SELECTORS.get(selector)
        if theme is None:
            raise SystemExit("semantic.css has an unexpected selector: " + selector)
        doc["semantic"][theme].update(decls)
    return json.dumps(doc, indent=2) + "\n"


def sync(dashboard, tailwind, out):
    src = os.path.join(dashboard, "src", "styles", "tokens")
    for name in ("primitives.css", "semantic.css"):
        shutil.copyfile(os.path.join(src, name), os.path.join(out, name))
    theme = read(tailwind)
    write(os.path.join(out, "zinc.css"), zinc_css(theme))
    write(os.path.join(out, "elevation.css"), elevation_css(theme))
    os.makedirs(os.path.join(out, "assets"), exist_ok=True)
    shutil.copyfile(os.path.join(dashboard, "src", "assets", "logo.svg"), os.path.join(out, "assets", "thoras-wordmark.svg"))
    write(os.path.join(out, "tokens.json"), tokens(out))


def main(argv):
    p = argparse.ArgumentParser(description="Copy the dashboard's design tokens into design/.")
    p.add_argument("dashboard", help="the dashboard folder of the platform repository")
    p.add_argument("--tailwind", help="Tailwind's theme.css (default: DASHBOARD/node_modules/tailwindcss/theme.css)")
    p.add_argument("--out", default=DESIGN, help="output folder (default: design/)")
    args = p.parse_args(argv[1:])
    tailwind = args.tailwind or os.path.join(args.dashboard, "node_modules", "tailwindcss", "theme.css")
    if not os.path.exists(tailwind):
        raise SystemExit("%s not found; run npm ci in the dashboard folder or pass --tailwind" % tailwind)
    os.makedirs(args.out, exist_ok=True)
    sync(args.dashboard, tailwind, args.out)
    print("wrote", os.path.relpath(args.out))


if __name__ == "__main__":
    main(sys.argv)
