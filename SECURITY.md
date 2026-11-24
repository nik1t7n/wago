# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub's private vulnerability reporting](https://github.com/nik1t7n/wago/security/advisories/new), not in a public
issue. Include what an attacker can do, the steps to reproduce, and the version (`wago version`). You will get an
answer within a few days; fixes for confirmed issues are released as soon as they are ready and credited in the
changelog unless you prefer otherwise.

## Supported versions

Security fixes go into the latest release.

## Deployment notes

- WaGo listens on loopback by default. Expose it only behind a reverse proxy with TLS or on a private network.
- The API key grants full control of every linked account. Pass it as a reference (`env:`, `file:`, `keychain:`),
  rotate it if it leaks, and use a long random value.
- `<home>/sessions/*/device.db` is the WhatsApp login itself. Anyone with the file can act as the account. Keep the
  state directory private (WaGo creates it with mode 0700) and its backups encrypted.
- Verify the `X-Webhook-Hmac` signature in every webhook receiver.
- New sessions cannot send until `sending` is turned on, and the policy's kill switch stops all sending at once.
