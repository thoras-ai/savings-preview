# Design

The report page follows the Thoras dashboard's design system. Its tokens are derived from the dashboard's token CSS (`dashboard/src/styles/tokens/` in the platform repository) and live in `design/`. This guide covers what the page needs from that system.

## Tokens

| File | What it holds |
| --- | --- |
| `design/primitives.css` | Raw values: the colour scales (`--primitive-brand-600`), spacing, radius, type sizes, weights, durations. Same in every theme. |
| `design/semantic.css` | Roles that change with the theme: `--surface-*`, `--text-*`, `--icon-*`, `--border-*`, `--hatch-*`, `--focus-ring`, `--chart-*`, and the theme-invariant `--fixed-*` set. |
| `design/zinc.css` | Tailwind's zinc scale (`--color-zinc-*`), which the semantic tokens reference. The dashboard gets it from Tailwind; the page has no Tailwind, so it carries a copy. |
| `design/elevation.css` | `--elevation-1` to `--elevation-5`: the dashboard's elevation utilities, which are Tailwind's `shadow-xs` to `shadow-xl` tinted with iris-900 at 20%. |
| `design/tokens.json` | Every token above by name and value, for tools and for reading. It is built from the CSS files. |
| `design/assets/thoras-wordmark.svg` | The wordmark from `dashboard/src/assets/logo.svg`. |

Do not edit these files by hand. To pick up token changes from the dashboard:

    cd ../platform/dashboard && npm ci        # once, for Tailwind's theme.css
    python3 scripts/sync_design_tokens.py ../platform/dashboard

The script copies `primitives.css`, `semantic.css` and the wordmark, takes the zinc scale and shadow sizes from `node_modules/tailwindcss/theme.css` (or `--tailwind FILE`), and rewrites `tokens.json`. Rebuild the page and check it in both themes. `tests/test_design_tokens.py` fails if `tokens.json` no longer matches the CSS, or if `src/styles/report.css` uses a token that is not defined. The dashboard's component token files (`tokens/components/`) are not copied, because the page uses none of them.

Page styles go in `src/styles/report.css` and use tokens, never literal colours, sizes or radii. Prefer a semantic token; use a primitive only where no role fits (the pill colours, for example).

## Copy

- Sentence case for every label, heading and button: "Annual savings", "Open another file".
- Number first, then the unit: "10.6 GiB", "$2,181", "35%". Figures that line up in columns or change in place use tabular numbers (`font-variant-numeric: tabular-nums`).
- Say what is missing rather than leaving a gap: "No estimate · Missing memory usage data", "1 of 4 workloads have too little data for a savings figure."
- Name the measure, not the system: "CPU to free", not "Thoras CPU recommendation".
- Address nobody. No exclamation marks, emoji or marketing tone. A warning says what happened and what the figure still covers.
- Use the product's terms: workload, namespace, cluster, waste, savings, right-sizing.

## Colour

- The page ground is `--surface-base` (warm off-white in light, zinc-950 in dark). Cards are `--surface-default`; popovers and tooltips `--surface-elevated`; tracks and hovered rows `--surface-inset`.
- Text is `--text-default`, with `--text-light` for secondary text and `--text-lighter` for captions. Hierarchy comes from size and weight, not from greying text out.
- Brand green: `--primitive-brand-600` for filled controls, `--text-brand` for links and brand text. In dark, filled controls use brand-300 with zinc-950 text.
- Ruby is for danger and waste only, mango for caution, azure for information and the focus ring, iris as the second accent.
- `--surface-brand`, `-info`, `-danger` and `-caution` stay light in dark mode. Put only the matching `--text-on-*` tokens on them.
- The header is dark in both themes and uses the `--fixed-*` tokens.

## Type

Roboto for all text; Roboto Mono for chart axes, table figures and code. Both are vendored in `src/fonts/` as latin subsets: Roboto 400, 500 and 700, Roboto Mono 400 and 500. The Roboto 400, 500 and 700 and Roboto Mono 400 files are byte-identical to the dashboard's; Roboto Mono 500 is used for bold table figures.

| Token | Size | Use |
| --- | --- | --- |
| `--primitive-text-xs` | 12px | Labels, table headers, chips, tooltips, captions |
| `--primitive-text-sm` | 14px | Body text on this page, tables, buttons |
| `--primitive-text-md` | 16px | Card titles |
| `--primitive-text-lg` | 20px | Section figures |
| `--primitive-text-xl` | 24px | Page title |
| `--primitive-text-2xl` | 32px | Hero numbers in the KPI tiles |

Use weight 500 (`--primitive-weight-medium`) for emphasis and headings. Bold (700) is reserved for highlighted counts inside a sentence.

## Spacing, radius and borders

- Spacing is on a 4px grid: `--primitive-space-4`, `-8`, `-12`, `-16`, `-20`, `-24`, `-40`.
- Radius: KPI tiles and panels `--primitive-radius-16`, cards `--primitive-radius-12`, tooltips `--primitive-radius-8`, alerts and fields `--primitive-radius-4`. Buttons, pills, segmented controls and progress bars are always `--primitive-radius-full`.
- Borders are 1px. `--border-lighter` for card edges and dividers, `--border-light` for inputs, `--border-default` for table header rules and outline buttons.

## Elevation

Cards and tiles at rest use `--elevation-1` with their border. Tooltips and menus use `--elevation-3`. In dark mode the shadow barely shows; a raised surface reads as raised because it is lighter (`--surface-default` is zinc-900, `--surface-elevated` zinc-800).

## Hatching

Diagonal 1px lines every 3px mark value that is potential rather than actual: waste, savings, spare capacity. Solid fills and lines are actuals. Line colours come from `--hatch-danger` (waste), `--hatch-brand` (savings) and `--hatch-neutral` (unallocated). The chart gap uses `--hatch-danger` in dark and ruby-200 in light, where ruby-100 is too faint on white.

## Charts

- Categorical series take `--chart-1` to `--chart-4`, then `--chart-other` for the remainder.
- Gridlines are dotted in `--border-lighter`. Tick labels are Roboto Mono 12px in `--text-light`.
- The page's chart compares requested (solid, `--chart-2`) with the target, usage plus 10% (dashed, `--chart-1`), and hatches the gap between them. In the CPU and memory views it also draws actual usage: the per-step peak as a solid `--text-default` line and the mean as a lighter `--chart-other` line. When the chart folds points to fit its width, peaks keep the highest value in each bucket and the other series take the mean.
- The dashboard's projected annual spend chart shows three scenarios over twelve months: without Thoras (dashed violet), current configuration (solid blue) and fully optimized (dashed teal). The band between the first two is hatched green (savings already captured) and the band between current and optimized is hatched red (waste still available). The legend carries each total. Those colours are hard-coded in the dashboard and have no tokens yet; the report's spend view uses the same idea with two lines, current spend and spend without waste.
- Every series is named in the legend, with a swatch that matches its stroke (solid, dashed or hatched).

## States

- Hover tints the background (`--surface-inset` on rows and cards, `--surface-brand` on brand controls) or darkens a filled control one step, over 150ms (`--primitive-duration-150`).
- Focus is a 2px `--focus-ring` outline with a 2px offset, on every control reachable by keyboard. A visually hidden file input keeps its label focusable.
- Disabled controls use `--surface-inset` with `--text-disabled`.
- Selected table rows use `--surface-brand` in light and brand-900 in dark.

## Themes

Light is the default. Dark sets `data-theme="dark"` on the root element, and the semantic tokens switch with it. The header has an Auto, Light and Dark switch: Auto follows the operating system and is the default, and a chosen theme is kept in `localStorage` where the browser allows it.

## Contrast

Measured on the token values:

- `--text-lighter` on `--surface-base` in light is 4.39:1, just under 4.5:1. Use it on `--surface-default` only, or for captions that carry nothing essential.
- `--focus-ring` (azure-400) on `--surface-default` in light is 2.5:1, under the 3:1 target for focus indicators. The 2px offset outline is the dashboard's choice; do not make it thinner.
- `--border-light` (1.5:1) and `--border-lighter` (1.3:1) are decorative. `--border-default` is 2.6:1. None of them is enough on its own to show where a control is; pair a border with a fill, text or an icon.

## Components

### Section bar

The report's sections are listed in a bar that sticks to the top of the page under the header, on `--surface-base` with a `--border-lighter` rule. Each item is the section number in Roboto Mono 12px `--text-lighter`, then its title in weight 500. The section in view is `--text-default` with a 2px brand-600 underline (brand-300 in dark); the others are `--text-light`. On narrow screens the bar scrolls sideways. Sections below the summary open with the same number and title at `--primitive-text-xl`.

### KPI tile

A card with a label, a value and a line of context. Label `--primitive-text-sm`, weight 500, `--text-light`; value `--primitive-text-2xl`, weight 500, tabular numbers; context `--text-light`. Tiles sit in an equal-width grid with 16px gaps and `--primitive-radius-16`. Keep a row to four tiles.

Variants: default; hero, for the headline figure, with the value in `--text-brand` (brand-300 in dark); provisional, for figures from too little history, with a 1px dashed mango-400 edge, no shadow, and the hero value in `--text-caution`. Provisional figures are rounded to two significant figures and marked with ~: "~$2,400".

### Confidence pill

A pill that says how far the data can be trusted: High confidence (brand) for tier 1, Good confidence (azure-50) for tier 2, Provisional (mango) for tier 3 and for a single snapshot.

### Banner

A notice across the content: `--surface-caution` with `--text-on-caution` and a mango-300 border for caution, `--surface-info` with azure-300 for information, `--surface-danger` with ruby-300 for danger. `--primitive-radius-4`. A bold lead word or phrase ("Provisional.") may come first, then one or two sentences. Danger banners are announced at once (`role="alert"`); the others politely (`role="status"`).

### Table

Dense rows inside a card. Header cells are 12px, weight 500, `--text-light`, over a 1px `--border-default` rule; rows have no rules, 8px by 12px padding and a `--surface-inset` hover. Figures are right-aligned in Roboto Mono with tabular numbers. Wrap the table in an `overflow-x: auto` container so it scrolls on narrow screens. Sortable headers are buttons with an arrow on the active column.

### Chart

An SVG inside a card, with the legend above it and a tooltip that follows the pointer. The tooltip lists every series at that time, then the gap, in `--surface-elevated` with `--elevation-3`. See Charts above for series and colours.

### Pill

A fully rounded chip for short states and counts, one to three words. Colour pairs are the 100 step of a scale with its 950 step as text: brand for the full-history tier, mango for warnings ("No estimate", limited history), zinc for neutral states ("Partial data"), azure-50 for informational tiers.

### Segmented control

A row of options in a fully rounded track (`--surface-inset` with a `--border-lighter` edge). The selected option is `--surface-default` with a 1px `--border-light` ring and weight 500. Used for the chart's Spend, CPU and Memory views, and, with the `--fixed-*` tokens, for the theme switch in the header.

### Capacity bar

A flat bar for one resource against a capacity: the nodes' allocatable CPU or memory, or the requests when the file has no nodes. Three layers start at the left edge: requests hatched in ruby-400, right-sized requests in iris-300 (iris-500 in dark), and usage in zinc-800 (zinc-200 in dark) on top. Right-sized requests are the target capped at each workload's current request, so requests minus right-sized requests is the waste. The track is `--surface-inset` with a 1px `--border-emphasized` edge, and a layer larger than the capacity stops at the edge. The figures sit to the right in Roboto Mono 12px `--text-light`, and the legend above names each layer with a matching swatch.

### Alert

A caution banner with one sentence. Put the count first, in bold, so the scope is clear: "**1 of 4 workloads** have too little data for a savings figure and are not in the totals."

### Print summary

What prints while a report is open: one letter-size document with half-inch margins, always in the light theme (the element carries `data-theme="light"`, so the semantic tokens resolve to light whatever the page theme). It repeats the summary's figures under the current settings: the cluster heading and confidence pill, the four headline figures in a two-column grid, confidence, what this means, spend over the window, the top workloads, and a paragraph on how the figure is computed. Cards are 1px `--border-light` boxes with `--primitive-radius-8` and no shadow, and none splits across a page break. Body text is `--primitive-text-xs`. Provisional data prints as on screen: the caution banner, dashed mango-400 edges and rounded figures with ~.
