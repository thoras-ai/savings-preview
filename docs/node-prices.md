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
4. **Spot.** A node whose `thoras.node.capacity_type` is `spot` is priced at the spot price. A spot node of a type with no spot price is not priced. The product does not detect Azure spot nodes, so they are priced at on-demand. The poll and Prometheus adapters mark a node spot from the labels the product reads: a `karpenter.sh/capacity-type` value that contains `spot` or `preemptible`, `eks.amazonaws.com/capacityType=spot`, `cloud.google.com/gke-spot=true` or `cloud.google.com/gke-preemptible=true`.
5. **Rates.** The per-core rate is the average priced node's hourly price over its average listed vCPUs, and the per-GiB rate the same over its listed GiB. A workload costs the larger of its CPU and its memory cost. Nodes that are not priced are left out of the averages, except nodes that were gone from the cluster when it was read, below.
6. **Fallback.** With no node priced, the rates come from the product's default node: 4 cores, 16 GiB, $0.192 per hour. This is what a minikube or kind cluster gets, since its nodes have no cloud provider ID or instance type.

In a file with several clusters, each cluster is priced from its own nodes with these rules, and its workloads with its cluster's rates. A fleet that spans clouds prices each cluster from its own cloud's table.

The levers under Assumptions override these rates: an instance type, typed rates, and a discount. The instance-type picker lists the same table, so a type picked in its cloud's reference region costs what the default pricing charges for an on-demand node of that type. In another region the picker scales the price by a factor per region (`src/js/prices.js`); the factors are approximate. The suggested rates ("Priced by their own cloud and region") scale each node's price by the same factors.

## Where the node attributes come from

The poll adapter reads each node's labels and provider ID from the Kubernetes API. The Prometheus adapter reads them from kube-state-metrics: the cloud from `kube_node_info`, and the instance type, region, capacity type and node pool from `kube_node_labels`. kube-state-metrics exports node labels only with `--metric-labels-allowlist`, so on most clusters the store has none.

When `run` or `prometheus` reaches a store through the Kubernetes API and the store leaves a node's instance type, cloud or capacity type unknown, it lists the nodes from the API once, on the same context, and fills in what the store lacks:

- the instance type from `node.kubernetes.io/instance-type`, then `beta.kubernetes.io/instance-type`;
- the region from `topology.kubernetes.io/region`;
- the cloud from `spec.providerID`;
- spot from the labels in step 4;
- the node pool from `karpenter.sh/nodepool`, `karpenter.sh/provisioner-name`, `eks.amazonaws.com/nodegroup`, `cloud.google.com/gke-nodepool`, `kubernetes.azure.com/agentpool` or `agentpool`.

The store's labels always win. A run that reads Datadog does the same when it has also read the kubeconfig to fall back to the cluster. If fewer than half of the nodes that report at the end of the data are in the API's list, the data and the kubeconfig are taken to be different clusters, since node names can repeat across clusters, and nothing is filled in. A store read with `--url`, and a run whose identity may not list nodes, keep the attributes the store gives. `thoras.node.type_source` records where each node's instance type came from, and `check` says where they would come from before a run.

### Nodes that are gone

Over a 14-day window an autoscaled cluster replaces many nodes, and the ones replaced are not in the live list. A node with no instance type that the API does not list is priced by the instance mix of the live nodes: it counts as one more node at the average hourly price, vCPUs and memory of the priced nodes in its own pool, when the store names the pool and the pool has a priced node, or else of all the priced nodes in the cluster. On a cluster where 12 of 535 nodes were live, the 523 others are priced by the mix of the 12. Without pools this gives the same rates as pricing the live nodes alone. The pricing line says how many nodes were priced this way, and Data and method says how many of the nodes were live. If no node can be typed, the default node applies as before.

## Differences from the product

- The product multiplies the average node's price by a calibration factor measured from the cluster's billed costs. The preview has no billing data, so it uses list prices.
- The product prices the nodes in its latest snapshot. The preview prices every node the file lists over its window, each once.
- For a custom machine type the product reads the node's CPU and memory capacity. The preview reads the size from the type name, which the capacity matches for CPU; memory capacity can be a little below the size in the name.
- The product always knows a node's cloud. The preview infers it from the instance type when the file does not say.
- The product reads every node it prices from the cluster. The preview prices the nodes that left the cluster during the window, when the store does not type them, by the live instance mix.

## Refreshing the table

When the platform adds a pricing migration, import it from an up-to-date checkout of the platform repository (its default branch) and commit the result with the changed figures noted in the CHANGELOG:

    python3 scripts/import_node_prices.py /path/to/platform
    make test

The script writes `internal/savings/node_prices.json` and prints how many types each cloud has. Tests that check figures against the table (`internal/savings/savings_test.go`, `tests/report_model.test.js`) name the instance types and prices they use, so a price change shows up there.
