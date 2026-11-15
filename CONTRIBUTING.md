# Contributing to k8s-buddy

Thanks for helping! Bug reports, new detectors, doctor checks and UI polish are all welcome.

## Contributor License Agreement

Before your first pull request can be merged, you'll need to sign the
[Contributor License Agreement](CLA.md) (CLA). It's adapted from the Apache Software Foundation's
individual CLA: you keep the copyright to your work and grant the maintainer a broad license to use,
modify, sublicense and distribute it, including under licenses other than the current one. That lets
the project change or add licenses later without having to track down every past contributor.

Signing takes one comment: when you open a pull request, a bot replies with a sentence to post back.
It records your GitHub username and the date, and the signature covers all your future contributions.
If you're contributing on behalf of your employer, make sure you're allowed to (section 4).

AI-assisted changes are fine; the person who opens the pull request signs the CLA and is responsible
for them.

## Development

Requirements: Go (version from `go.mod`; the `go` command downloads it automatically) and, for the
native macOS UI, Xcode command line tools.

```bash
make run        # native macOS app from the terminal
make headless   # any OS: prints a URL that opens the UI in your browser
make app        # builds dist/k8s-buddy.app
make test       # go test ./...
make vet        # go vet ./...
```

The web UI is plain ES modules in `internal/server/web/`, embedded into the binary, so there's no build
step. Before sending a change to `app.js`, check it parses as a module:

```bash
cp internal/server/web/app.js /tmp/app.mjs && node --check /tmp/app.mjs
```

### Test against a throwaway cluster

Please don't develop against clusters you care about. `testdata/demo/` holds deliberately broken
workloads that trigger the detectors and doctor checks. See *Testing against a throwaway cluster* in the
[README](README.md) for a k3s-in-Docker setup.

## Ground rules

- **Monitoring stays read-only.** Detectors and doctor checks only list and watch. Anything that changes
  a cluster goes through the console, which asks the user to confirm.
- **Never retain secret values.** Secrets are redacted as they're received; keep it that way.
- **Keep the core portable.** Only `internal/ui/ui_darwin.m` is macOS-specific; CI builds and tests on
  macOS, Windows and Linux.
- **Alerts should say why.** Prefer a specific cause and a next step over a generic symptom, and dedupe
  per workload rather than per pod.
- Add a unit test for new detectors and checks (see `internal/monitor/*_test.go` for fake listers).

## Pull requests

1. Fork, branch from `main`, keep the change focused.
2. `go vet ./...` and `go test ./...` pass, and you've signed the CLA.
3. Describe what you changed and how you tested it; screenshots help for UI changes.
