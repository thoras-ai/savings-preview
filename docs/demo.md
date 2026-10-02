# Five-minute demo

A walk through the report with the demo cluster: what to click, in what order, and what to say. It takes about five minutes.

## The demo cluster

`go run ./scripts/gen_demo` writes `fixtures/demo-cluster.compact.otlp.json`, a cluster called `prod-us-east-1`: 60 workloads in 8 namespaces (web, api, workers, data, batch, platform, monitoring, kube-system) on 28 AWS nodes in four groups (31 over the window, counting spot replacements): m6i.2xlarge and r6i.2xlarge on demand, m6i.large for system pods, and an m6i.xlarge spot group that loses three nodes to reclamation. It covers 14 days at 5-minute steps, from a Prometheus source, written as the compact file `run` writes for the report.

What is in it:

- Web and api traffic peaks at 18:00 UTC on weekdays and runs at about 60% at weekends. The two storefront deployments scale their replicas with it.
- Workers follow the same day with short queue bursts on top.
- Batch jobs are idle most of the day: four run for two and a half hours after 01:00 UTC every night, three for six hours on Sunday morning. Their requests stay reserved around the clock.
- Most workloads ask for well over their peak. A few are right-sized (`postgres-orders`, `redis-sessions`), and a few ask for less than they use (`opensearch` memory, `catalog-api` CPU).
- `search-indexer` and `image-processor` leak memory up to their limit and are OOM killed four times each.
- `users-api` had its CPU request cut from 2 cores to 0.5 on day 10.

Run it from the repository root. The output is the same on every run, so the file is not committed; the Pages build and CI generate it. `-full` writes the 5-minute file instead of the compact one.

## Before the demo

Open the published page and choose **Demo cluster** under "Or try a sample", or build it locally:

    go run ./scripts/gen_demo
    python3 scripts/build_report.py --site dist
    python3 -m http.server -d dist

Set the theme to Light if the room has a projector. Open the report once beforehand so the file is cached; it is about 20 MB.

The figures below are at the default settings. They hold as long as the generator does not change.

## 1. Summary (1 minute)

Show: the four tiles, the confidence pill, "What this means", and the spend chart.

- "This is a 60-workload production cluster. The CLI read 14 days of usage from the Prometheus it already runs. Nothing was installed in the cluster, and nothing left the laptop."
- "It spends about $73,000 a year on what its workloads request. About $41,000 of that, 56%, is requested above what the workloads used plus headroom."
- "That is about 103 of 149 requested cores and 210 of 465 GiB."
- Point at the confidence pill: "Good confidence: two weeks at 5-minute resolution. With 1-minute data it would be high."
- Point at the spend chart: the gap between current spend and "without waste" is widest at night and at weekends, when traffic drops and the requests do not.

## 2. Where the money is (1 minute)

Show: the map, the namespace ranking and the top 10.

- "Every cell is a workload, sized by what it would save."
- "web and batch lead. Batch is the interesting one: `nightly-etl` and `ml-feature-backfill` run for a few hours and hold their requests all day. A static request has to cover the busiest hour; Thoras sets requests for the hour you are in."
- "data spends the most and saves the smallest share of it. The databases are close to right-sized already, and the report says so instead of inventing savings."
- Switch the map to Spend to show the difference between where the money goes and where the waste is.

## 3. A workload drill-down (1 minute 30)

Select `storefront` in the top 10.

- "Requested, usage and target over two weeks. The replicas follow traffic, so total requests move with them, but each pod still asks for far more than it uses."
- "What Thoras would set: the per-pod request it would have averaged, next to the single peak-safe value a person would have to pick to be safe at the busiest hour."

Then go back to All workloads, filter for `search-indexer`, and open it.

- "This one is the other side. Memory climbs until it hits the 4 GiB limit and the pod is killed: four OOM kills, marked on the chart."
- "The risk read says the limit is below the peak-safe value. Thoras raises the limit with the request instead of cutting memory on a workload that is already being killed."

If there is time, `users-api` shows a request someone cut by hand on day 10: the requested line drops and the waste shrinks from that point.

## 4. Levers (1 minute)

Open Assumptions.

- Raise headroom to 20% and the peak window to 1 hour: "A more conservative target still saves about $37,700, 52%. The figure does not depend on cutting it fine."
- Set a 30% spot or committed-use discount: "Your prices, not list prices. Spend and savings drop by the same share; the percentage stays at 56%."
- Untick batch under Namespaces: "Leave out what you do not want counted. Without batch it is about $30,000."
- "Every setting is in the link. Copy link shares this exact view." Reset to defaults before moving on.

## 5. Print (30 seconds)

Select Print summary in the header.

- "One letter-size page for the budget owner: the headline figures, the confidence, spend over the window, the top workloads and how the figure is computed, under the settings on screen."
- Close by pointing at Data and method: "Every number traces back to the file and the formula on this page. To run it on your cluster, it is one command with your existing kubeconfig."
