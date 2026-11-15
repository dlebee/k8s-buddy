# k8s-buddy

A little floating friend that watches **every Kubernetes context in your kubeconfig** while you work, and nudges you when something needs a look.

- **Floating buddy** — sits on top of your windows, bounces and shows a count when something new breaks. Click it for the alert history; drag it anywhere; right-click for the menu.
- **Menu bar icon** — `⎈ 3` style count, overview, settings, show/hide the buddy.
- **Multi-cluster overview** — health of all your clusters at a glance.
- **Resources** — browse pods, deployments, statefulsets, daemonsets, services, PVCs, nodes and warning events across all clusters, with filters, YAML and copy-ready `kubectl` commands.
- **Doctor** — an on-demand checkup for things that are quietly wrong (dangling services and ingresses, PDBs that block upgrades, orphaned volumes, …).

It is read-only: it only lists and watches.

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

Alerts are deduplicated, state-based ones resolve on their own when the condition clears, and you can **snooze** (default 4h) or **mute** any alert.

## Doctor checks

Services with no pods · ingresses pointing at missing services/ports or empty services · PodDisruptionBudgets allowing 0 disruptions · HPAs targeting missing workloads · Released/Failed PersistentVolumes · bound PVCs no pod mounts · leftover failed/evicted pods · pods without a controller.

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
- Windows (WebView2 + tray) and Linux shells.
- Launch at login, notifications, more doctor checks, custom rules.

## License

MIT
