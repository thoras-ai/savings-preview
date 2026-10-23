# Sending Thoras your savings preview data

Savings preview estimates how much Thoras would save on your Kubernetes clusters. It reads the CPU and memory usage history your clusters already keep, compares what each workload requests with what it actually uses, and works out what the clusters would cost with Thoras sizing those requests.

These steps produce one file, `savings-preview.zip`, that Thoras turns into your savings report. They take about 10 minutes, most of it waiting. The tool only reads: it installs nothing in your clusters and changes nothing there, and it uploads nothing. You send the zip yourself.

## What you need

- A Mac or Linux machine with the `kubectl` access you already use for the clusters
- Homebrew, Node.js 20 or later, or `curl`

## 1. Install

With Homebrew:

    brew install thoras-ai/tap/savings-preview

Or, with Node.js 20 or later, write `npx @thoras-ai/savings-preview@latest` wherever the steps below say `savings-preview`.

Or use the install script:

    curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | sh

Check it worked:

    savings-preview --version

## 2. Find your cluster contexts

    kubectl config get-contexts

Pick the two or three production clusters you want in the report. The steps below call them `prod-1`, `prod-2` and `prod-3`.

The tool needs no Thoras account. It uses your existing `kubectl` credentials, so sign in to your cloud first the way you normally do (for example `aws sso login` on EKS, `gcloud auth login` on GKE, or `az login` on AKS). Then make sure `kubectl` can reach each cluster:

    kubectl --context prod-1 get nodes

If that works, the tool will be able to connect too.

## 3. Check access (optional, a few seconds)

This reads no history and writes nothing. It lists what it will ask each cluster and waits for Enter:

    savings-preview check --context prod-1 --context prod-2 --context prod-3

Each cluster ends with one of three answers:

- `ready`: it will work.
- `limited`: it will work, with less history or detail. The line says why.
- `cannot run`: the line names the missing permission.

## 4. Collect the data

Run it in a new, empty folder:

    mkdir savings-preview-data && cd savings-preview-data
    savings-preview --context prod-1 --context prod-2 --context prod-3 --collect-only

It first prints what it found in each cluster and what it will read and write, then waits. Press Enter to start, or `q` to quit without reading anything.

It reads 14 days of CPU and memory history from each cluster's Prometheus, a few clusters at a time. This usually takes a minute or two. When it finishes, the last line reads:

    Send savings-preview.zip to your Thoras contact. It holds the report and the data, and nothing else from your environment.

## 5. Send us the zip

Send `savings-preview.zip` to your Thoras contact. You don't need to send the larger `.otlp.json` files in the folder.

Send the zip; if you only have the larger .otlp.json files, send those and we can build the report from them. [Building a report from data files](large-files.md) explains how.

## What the zip contains

- For each cluster: workload and namespace names, node instance types, and CPU and memory requests and usage at 15-minute steps
- A summary and a copy of the report

It contains no Secrets, environment variables, logs, container images, annotations or IP addresses. The data is plain JSON, so you can open it and read it before you send it.

## What the tool reads

It uses your existing `kubectl` credentials and sends only reads:

- `list` on services, to find Prometheus
- `get` on `services/proxy`, to query Prometheus through the API server
- When a cluster has no Prometheus, `list` on pods, workloads and nodes, plus metrics-server readings

The full list of requests, with read-only service accounts you can use instead of your own credentials, is in [security.md](security.md). [cluster-load.md](cluster-load.md) explains why a run does not affect your workloads.

## If something goes wrong

Save the check output and send it to us:

    savings-preview check --yes --context prod-1 --context prod-2 --context prod-3 > savings-preview-check.txt
