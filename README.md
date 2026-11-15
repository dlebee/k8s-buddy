# k8s-buddy

A little floating friend that watches **every Kubernetes context in your kubeconfig** while you work, and nudges you when something needs a look.

- **Floating buddy** — sits on top of your windows, bounces and shows a count when something new breaks. Click it for the alert history; drag it anywhere; right-click for the menu.
- **Menu bar icon** — `⎈ 3` style count, overview, settings, show/hide the buddy.
- **Multi-cluster overview** — health of all your clusters at a glance.
- **Resources** — browse workloads, networking, storage (PVCs, PVs, storage classes), config (ConfigMaps, Secrets — values never loaded), Helm releases, nodes, events and **any custom resource** across all clusters. Every object, alert and doctor finding opens with `kubectl describe` output, YAML and a built-in **console** to run the suggested commands.
- **Doctor** — an on-demand checkup for things that are quietly wrong (dangling services and ingresses, PDBs that block upgrades, orphaned volumes, …).

Monitoring is read-only: it only lists and watches. The console runs `kubectl`, `helm` and `cmctl` directly (no shell, no pipes, no interactive commands) with your kubeconfig, and asks before anything that changes a cluster.

## What it watches

| Signal | How |
| --- | --- |
| CrashLoopBackOff, ImagePullBackOff, CreateContainerConfigError, … | pod container waiting state, grouped per workload |
| Container restarts (incl. OOMKilled) | restart count increases, grouped per workload/container |
| Pods unschedulable for > 5m | `PodScheduled=False/Unschedulable` |
| PVC stuck `Pending` (> 2m) or `Lost` | PVC phase |
| `LoadBalancer` service without external IP (> 3m) | service status |
| Node `NotReady`, Memory/Disk/PID pressure, cordoned | node conditions |
| Node pool rolling to a new kubelet version / OS image | new values per pool, mixed versions per pool |
| Control plane upgrade | `/version` polled every 5m |
| Selected warning events | FailedMount, FailedAttachVolume, Evicted, SystemOOM, BackoffLimitExceeded, SyncLoadBalancerFailed, … |
| Cluster unreachable | connection / auth failures |
| Scheduled VM maintenance (freeze / reboot / redeploy) | node-problem-detector events (AKS and others) |
| Repeated readiness / liveness probe failures | `Unhealthy` events (3+ failures) |
| cert-manager: certificate **not renewing on time**, expired, not ready; issuer not ready | Certificate / Issuer / ClusterIssuer status |
| Helm release failed or stuck in `pending-*` | Helm release secrets' labels (payload never read) |
| Ingress never got an address | ingress status (wrong class, controller down, or ignored by NGINX — with the reason) |

Alerts are deduplicated, state-based ones resolve on their own when the condition clears, and you can **snooze** (default 4h) or **mute** any alert.

## Doctor checks

Services with no pods · ingresses pointing at missing services/ports or empty services · ingresses using a missing IngressClass · NGINX mergeable ingresses that are silently ignored (minions without a master, empty masters, hosts claimed twice — this also catches cert-manager HTTP-01 solvers that can never succeed) · PodDisruptionBudgets allowing 0 disruptions · HPAs targeting missing workloads · Released/Failed PersistentVolumes · bound PVCs no pod mounts · leftover failed/evicted pods · pods without a controller.

## Install / run (macOS)

Requires Go 1.27+ and Xcode command line tools.

```bash
make app            # builds dist/k8s-buddy.app (menu bar app, no Dock icon)
open dist/k8s-buddy.app
```

Or run from a terminal:

```bash
make run            # native UI
make headless       # no native UI; prints a URL to open the UI in a browser (any OS)
```

Flags: `--kubeconfig PATH`, `--data-dir DIR`, `--headless`, `-v`.

Settings and history live in `~/Library/Application Support/k8s-buddy/`. Contexts you delete from your kubeconfig are forgotten automatically; stale ones you want to keep can be unchecked in **Settings**.

Kubeconfig exec plugins (`az`/`kubelogin`, `aws`, `gke-gcloud-auth-plugin`, `doctl`, …) work when launched from Finder too: the app borrows your login shell's `PATH`.

## Testing against a throwaway cluster

`testdata/demo/` contains deliberately broken workloads that trip every detector and doctor check:

```bash
docker run -d --name k8s-buddy-k3s --privileged -p 127.0.0.1:16443:6443 rancher/k3s:v1.33.5-k3s1 server --tls-san 127.0.0.1
docker exec k8s-buddy-k3s cat /etc/rancher/k3s/k3s.yaml | sed 's/6443/16443/' > /tmp/k3s.yaml
KUBECONFIG=/tmp/k3s.yaml kubectl apply -f testdata/demo/broken.yaml
./bin/k8s-buddy --headless --kubeconfig /tmp/k3s.yaml --data-dir /tmp/buddy-demo
```

## How it works

```
kubeconfig ─▶ manager ─▶ one monitor per context (client-go informers + /version poll)
                              │ detectors (state sweeps every 20s + event handlers)
                              ▼
                        alert store (dedupe, snooze, mute, filters, history on disk)
                              │
            127.0.0.1 HTTP API + SSE (random port, per-launch token)
                              │
      native shell (Cocoa + WKWebView): buddy · alert panel · overview window · menu bar
```

- `internal/monitor` — informers, detectors, resources, doctor
- `internal/alert` — store and the `Filter` interface
- `internal/server` — local API and the embedded web UI
- `internal/ui` — the macOS shell (`ui_darwin.m`) and a headless fallback

## Roadmap

- **Bring your own model**: the `alert.Filter` interface is where an LLM can review deterministic alerts and drop noisy false alarms before they nudge you.
- Windows (WebView2 + tray) and Linux shells. Everything except `internal/ui/ui_darwin.m` is portable; CI builds and tests on macOS, Windows and Linux, and `--headless` already works everywhere.
- Launch at login, notifications, more doctor checks, custom rules.

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md). First-time contributors sign a
short [CLA](CLA.md) by posting a comment on their pull request.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
