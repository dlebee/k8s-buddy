# Notes for coding agents

This repository is public. See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test and send changes.

## Never copy real cluster data into the repo

k8s-buddy is often developed while connected to real clusters. Nothing from them may end up in a
tracked file: code, tests, comments, testdata, docs, screenshots or commit messages.

That includes cluster and context names, namespaces, workload/ingress/service/Helm release names,
hostnames and domains, IP addresses, node and VM names, cloud subscription/tenant/resource group IDs,
company, product or customer names, and usage numbers lifted from a live cluster.

When a bug was found on a real cluster, reproduce it with made-up data:

- Names: generic ones (`shop`, `api`, `web`, `store`, `db`, `a`, `b`) or the demo namespace `buddy-demo`.
- Hosts: `*.example.com` or `*.buddy.test`.
- IPs: documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`) or `10.0.0.x`.
- Comments describe the scenario ("master shadowed by an older plain ingress"), not where it came
  from ("the case from our cluster").

Before committing, check the diff for anything that came from `kubectl` output, logs or the running
app, and replace it.
