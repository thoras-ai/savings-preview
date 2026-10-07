# Node prices

The report and `summary.json` price nodes the way the Thoras product does, from a copy of the product's price table: `internal/savings/node_prices.json`. The Go summary embeds it, and both page builders inline it ahead of `src/js/model.js`, so the page and the CLI read the same prices.

## Where the table comes from

The product loads its `instance_pricing` table from SQL migrations in the platform repository's `go/db/migrations`. `scripts/import_node_prices.py` reads the newest file of each kind:

| Migration | What it holds |
| --- | --- |
| `*_aws_pricing.sql` | AWS on-demand prices and spot discounts, us-east-1 |
| `*_gcp_pricing.sql` | Google Cloud on-demand prices and spot discounts, us-central1 |
| `*_azure_pricing.sql` | Azure on-demand prices and spot discounts, eastus |
| `*_gcp_custom_node_pricing.sql` | Google Cloud custom machine types, per vCPU and per GiB, on-demand and spot |

The platform generates the first three with `scripts/generate-cloud-pricing`, one region per cloud, and stores every row under region `default`. The import keeps Linux rows only, since the product prices every node as Linux, and computes each spot price as the migrations do: on-demand × (100 − discount) / 100, rounded to 10 decimal places. `source` in the JSON names the files it read.

## How a node is priced

The rules follow `CurrentAverageNode` in the platform's `go/internal/repositories/node_details.go`.

1. **Cloud.** `cloud.provider` picks the table: `aws`, `gcp` or `azure`, which the adapters take from the node's provider ID (`aws://`, `gce://`, `azure://`). A node on any other cloud is not priced. When the file does not name the cloud (`unknown`, as some stores report it), the cloud whose table has the instance type is used. No type is in more than one table.
2. **Instance type.** `host.type` is looked up in that cloud's table. A match gives the listed vCPUs, memory and hourly price.
3. **Custom types.** A Google Cloud type that is not in the table but starts with a custom family prefix, such as `n2d-custom-`, is priced per vCPU and per GiB. The vCPUs and memory come from the type name (`n2d-custom-16-32768` is 16 vCPUs and 32 GiB), or from the node's allocatable CPU and memory when the name does not give them.
4. **Spot.** A node whose `thoras.node.capacity_type` is `spot` is priced at the spot price. A spot node of a type with no spot price is not priced. The product does not detect Azure spot nodes, so they are priced at on-demand. The poll and Prometheus adapters mark a node spot from the labels the product reads (`karpenter.sh/capacity-type`, `eks.amazonaws.com/capacityType`, `cloud.google.com/gke-spot`, `cloud.google.com/gke-preemptible`).
5. **Rates.** The per-core rate is the average priced node's hourly price over its average listed vCPUs, and the per-GiB rate the same over its listed GiB. A workload costs the larger of its CPU and its memory cost. Nodes that are not priced are left out of the averages.
6. **Fallback.** With no node priced, the rates come from the product's default node: 4 cores, 16 GiB, $0.192 per hour. This is what a minikube or kind cluster gets, since its nodes have no cloud provider ID or instance type.

The levers under Assumptions override these rates: an instance type from the page's own catalog, typed rates, and a discount. The suggested rates ("Priced by their own cloud and region") scale each node's price by the catalog's factor for the node's region.

## Differences from the product

- The product multiplies the average node's price by a calibration factor measured from the cluster's billed costs. The preview has no billing data, so it uses list prices.
- The product prices the nodes in its latest snapshot. The preview prices every node the file lists over its window, each once.
- For a custom machine type the product reads the node's CPU and memory capacity. The preview reads the size from the type name, which the capacity matches for CPU; memory capacity can be a little below the size in the name.
- The product always knows a node's cloud. The preview infers it from the instance type when the file does not say.

## Refreshing the table

When the platform adds a pricing migration, import it from an up-to-date checkout of the platform repository (its default branch) and commit the result with the changed figures noted in the CHANGELOG:

    python3 scripts/import_node_prices.py /path/to/platform
    make test

The script writes `internal/savings/node_prices.json` and prints how many types each cloud has. Tests that check figures against the table (`internal/savings/savings_test.go`, `tests/report_model.test.js`) name the instance types and prices they use, so a price change shows up there.
