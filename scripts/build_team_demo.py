#!/usr/bin/env python3
"""Build docs/team-demo/index.html from the slide files in docs/team-demo/slides.

Each slide is one <section> on a 1920x1080 canvas with inline styles. The page
shows the slides in deck.json's order, scaled to the window, with each slide's
speaker notes (its <aside>) underneath. Arrow keys or space step through them.

    python3 scripts/build_team_demo.py
"""
import html
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DECK = os.path.join(ROOT, "docs", "team-demo")

PAGE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
{fonts}
<style>
  body {{ margin: 0; background: #27272a; font-family: Roboto, Arial, sans-serif; }}
  .slide {{ margin: 24px auto; width: 1920px; transform-origin: top left; }}
  .frame {{ width: 1920px; height: 1080px; position: relative; overflow: hidden; box-shadow: 0 4px 24px rgba(0,0,0,.4); }}
  .frame > section {{ position: absolute; inset: 0; box-sizing: border-box; }}
  .frame h1, .frame h2, .frame h3, .frame p, .frame ul {{ margin: 0; }}
  .frame table {{ width: 100%; border-collapse: collapse; }}
  .frame th, .frame td {{ padding: 0.35em 0.6em; border-bottom: 1px solid #e4e4e7; text-align: left; }}
  x-icon {{ display: none; }}
  x-connector {{ display: flex; align-items: center; justify-content: center; flex: none; }}
  x-connector::after {{ content: "\\2192"; font-size: 48px; color: #9f9fa9; }}
  .notes {{ color: #d4d4d8; font-size: 28px; line-height: 1.5; padding: 24px 8px 48px; }}
  .notes b {{ color: #ffffff; }}
</style>
</head>
<body>
{slides}
<script>
  // Scale the 1920px slides to the window, and step through them with the keyboard.
  const slides = [...document.querySelectorAll('.slide')];
  function fit() {{
    const s = Math.min(1, (window.innerWidth - 48) / 1920);
    for (const el of slides) {{
      el.style.transform = 'scale(' + s + ')';
      el.style.marginBottom = (-(1 - s) * el.offsetHeight + 24) + 'px';
      el.style.marginLeft = Math.max(24, (window.innerWidth - 1920 * s) / 2) + 'px';
    }}
  }}
  window.addEventListener('resize', fit); fit();
  document.addEventListener('keydown', e => {{
    const step = ['ArrowRight', 'ArrowDown', 'PageDown', ' '].includes(e.key) ? 1 : ['ArrowLeft', 'ArrowUp', 'PageUp'].includes(e.key) ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const y = window.scrollY + 1;
    const tops = slides.map(el => el.getBoundingClientRect().top + window.scrollY);
    let i = tops.findIndex(t => t >= y);
    i = step > 0 ? (i < 0 ? slides.length - 1 : i) : Math.max(0, (i < 0 ? slides.length : i) - 2);
    window.scrollTo({{ top: tops[i] - 24, behavior: 'smooth' }});
  }});
</script>
</body>
</html>
"""


def main():
    with open(os.path.join(DECK, "deck.json"), encoding="utf-8") as f:
        deck = json.load(f)
    fonts = "\n".join('<link rel="stylesheet" href="%s">' % html.escape(face["href"], quote=True)
                      for face in deck.get("faces", {}).values() if face.get("href"))
    parts = []
    for n, sid in enumerate(deck["order"], 1):
        with open(os.path.join(DECK, "slides", sid + ".html"), encoding="utf-8") as f:
            section = f.read().strip()
        m = re.search(r"<aside>(.*?)</aside>", section, re.S)
        notes = m.group(1).strip() if m else ""
        section = re.sub(r"\s*<aside>.*?</aside>", "", section, flags=re.S)
        parts.append('<div class="slide" id="%s"><div class="frame">%s</div>'
                     '<div class="notes"><b>%d.</b> %s</div></div>' % (sid, section, n, notes))
    out = PAGE.format(title=html.escape(deck["title"]), fonts=fonts, slides="\n".join(parts))
    with open(os.path.join(DECK, "index.html"), "w", encoding="utf-8") as f:
        f.write(out)
    print("wrote docs/team-demo/index.html (%d slides)" % len(deck["order"]))


if __name__ == "__main__":
    sys.exit(main())
