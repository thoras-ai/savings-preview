# @thoras/savings-preview

Runs [Thoras Savings Preview](https://github.com/thoras-ai/savings-preview), a read-only CLI that reads a Kubernetes cluster's usage history and shows what Thoras would have saved.

    npx @thoras/savings-preview check --context my-cluster
    npx @thoras/savings-preview --context my-cluster

The package holds no binary. On first use it downloads the release archive of the same version for your machine (macOS or Linux, x64 or arm64) from GitHub, checks it against the release's `checksums.txt`, and keeps the binary in `~/Library/Caches/savings-preview` on macOS or `~/.cache/savings-preview` on Linux. Set `SAVINGS_PREVIEW_CACHE` to keep it elsewhere.

It downloads with `GITHUB_TOKEN`, `GH_TOKEN` or the token of a logged-in [gh](https://cli.github.com), in that order. The token needs read access to `thoras-ai/savings-preview`.

`npm install -g @thoras/savings-preview` installs a `savings-preview` command that does the same. See the [getting started guide](https://github.com/thoras-ai/savings-preview/blob/main/docs/getting-started.md) for what the CLI reads and writes.
