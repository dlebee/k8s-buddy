#!/usr/bin/env bash
# Stores the macOS signing + notarization secrets for the release workflow in GitHub.
# Run it yourself: it prompts for passwords and never prints them.
#
#   1. Keychain Access → login → My Certificates → "Developer ID Application: …" → right-click →
#      Export → save as .p12 and choose a password.
#   2. Create an app-specific password at https://account.apple.com (Sign-In and Security).
#   3. packaging/macos/set-signing-secrets.sh ~/Desktop/developer-id.p12
set -euo pipefail

REPO="${REPO:-dlebee/k8s-buddy}"
P12="${1:?usage: $0 path/to/developer-id.p12}"
[ -f "$P12" ] || { echo "no such file: $P12" >&2; exit 1; }

read -r -s -p "Password you chose when exporting the .p12: " P12_PASSWORD; echo

# Prove the file holds a usable Developer ID identity before uploading anything.
KC="$(mktemp -d)/check.keychain-db"
KC_PW="$(openssl rand -hex 16)"
security create-keychain -p "$KC_PW" "$KC" >/dev/null
trap 'security delete-keychain "$KC" 2>/dev/null || true' EXIT
security unlock-keychain -p "$KC_PW" "$KC"
if ! security import "$P12" -k "$KC" -P "$P12_PASSWORD" -f pkcs12 >/dev/null 2>&1; then
  echo "Couldn't open the .p12 — wrong password?" >&2; exit 1
fi
IDENTITY="$(security find-identity -v -p codesigning "$KC" | sed -nE 's/.*"(Developer ID Application: [^"]+)".*/\1/p' | head -1)"
[ -n "$IDENTITY" ] || { echo "The .p12 has no Developer ID Application identity." >&2; exit 1; }
echo "Found: $IDENTITY"

base64 -i "$P12" | gh secret set MACOS_CERT_P12_BASE64 -R "$REPO"
printf '%s' "$P12_PASSWORD" | gh secret set MACOS_CERT_PASSWORD -R "$REPO"
gh variable set MACOS_SIGNING_IDENTITY -R "$REPO" -b "$IDENTITY"
unset P12_PASSWORD

read -r -p "Apple ID email (for notarization): " APPLE_ID
read -r -s -p "App-specific password: " APP_PASSWORD; echo
printf '%s' "$APPLE_ID" | gh secret set AC_APPLE_ID -R "$REPO"
printf '%s' "$APP_PASSWORD" | gh secret set AC_APP_PASSWORD -R "$REPO"
unset APP_PASSWORD

echo "Done. Secrets in $REPO:"
gh secret list -R "$REPO"

read -r -p "Delete $P12 now? [Y/n] " ans
case "$ans" in n|N) echo "Kept $P12 — delete it when you're done." ;; *) rm -P "$P12" 2>/dev/null || rm -f "$P12"; echo "Deleted." ;; esac
