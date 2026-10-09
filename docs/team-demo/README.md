# Team demo slides

The 13 slides from the internal team demo of savings preview (October 9, 2026): what it is, how to run it, a live run against three clusters, running across many clusters, getting results back from a customer, and status.

Open `index.html` in a browser. It shows the slides in order, scaled to the window, with each slide's speaker notes under it. Arrow keys and space step through them.

## Editing

- `slides/<id>.html` holds one slide: a single `<section>` on a 1920×1080 canvas, every style inline, with the speaker notes in a final `<aside>`.
- `deck.json` holds the order, the outline and the fonts.

After changing either, rebuild the page:

```bash
python3 scripts/build_team_demo.py
```

The figures on the results slides come from a dry run on October 8, priced at the default node (4 cores, 16 GiB, $0.192 an hour), before instance types were read from the Kubernetes API.
