# Direct admin listener (emergency access)

> **Superseded 2026-10-19.** The direct admin listener (`ADMIN_LISTEN`,
> `ADMIN_NETWORKS`, `ADMIN_TLS_*`) was removed, with its code, tests and e2e.
> Reason: the maintainer decided that break-glass is a local admin on the
> normal login page plus the host CLI `tasmofleet user passwd`; a second listener
> with its own certificate, cookies and network gate is more machinery than a
> homelab needs ("it is not Fort Knox"). The only protection on the login page is
> the existing rate limiting (`login-rate-limiting.md`). This document is kept as
> the record of the decision; none of the claims below (X1 to X14) applies any
> more. The `sessions.listener` column from migration 0003 stays in the schema
> (migrations are append-only) but nothing reads or writes it. Migration `0011_drop_admin_sessions.sql` deletes the sessions the removed listener had created, because without the listener check the main listener would accept the same cookie value.

Scope: the second UI listener `ADMIN_LISTEN` (plan M2 H-1). It lets a local
admin in when the IdP or the reverse proxy is down. Code:
`internal/httpx/acceptfilter.go`, `internal/httpx/tls.go`, `internal/auth`
(`Options.Listener`, `session.go`, `local.go`), `internal/app/adminlistener.go`,
`internal/store` (migration `0003_admin_listener.sql`), `internal/config`.
Operator procedure: `docs/runbooks/emergency-access.md`.

This document extends `login-rate-limiting.md` (C1 to C14) and
`account-lifecycle.md` (L1 to L14); it does not weaken them. Every claim
(X1 to X13) has a test. Review findings are judged against these three
documents.

## Problem

Break-glass is useless while Authentik is down: Traefik's forward auth sits in
front of the public name, and the direct LAN URL cannot hold the `Secure` /
`__Host-` cookies over plain HTTP. The fix must not become a second, weaker
way in.

## Model

- **Listener**: `ADMIN_LISTEN` (empty = off). One more `http.Server` in the same
  process with the same store, accounts and web handler, but its own
  `auth.Service` (`Options.Listener = "admin"`, `Mode = local`).
- **Network gate**: `httpx.NetworkFilter` wraps the TCP listener. A connection
  whose peer is outside `ADMIN_NETWORKS` is closed in `Accept`, before TLS:
  no handshake, no HTTP, no bytes. Only the TCP peer counts.
- **Always HTTPS**: `ADMIN_TLS_CERT_FILE`/`ADMIN_TLS_KEY_FILE`, otherwise a
  self-signed ECDSA P-256 certificate in `<DATA_DIR>/tls/admin/` (key 0600, one
  year, renewed 30 days before expiry, reused across restarts). SANs: `localhost`,
  loopback, the host name, `TLS_SANS`, `ADMIN_TLS_SANS`. The SHA-256 fingerprint
  is logged whenever a certificate becomes the served one (start, renewal,
  reload), shown live on `/about`, and printed by `tasmofleet tls fingerprint`.
- **Local login only**: no OIDC route, no header authentication, no
  `TRUSTED_PROXIES`, base path `/`. `X-Forwarded-*` and `X-Ingress-Path` are
  not read at all (the listener's `httpx.Proxy` has no trusted peers).
- **Own origin state**: session cookie `__Host-tasmofleet-admin`, pre-session
  CSRF cookie `__Host-tasmofleet-admin-csrf`, device cookie
  `__Host-tasmofleet-admin-device`; all Secure, HttpOnly, `Path=/`, no `Domain`.
  The HMAC keys (device cookie, CSRF, flow) are derived under a
  listener-specific label. Sessions carry `sessions.listener` (`main` or
  `admin`, migration 0003, default `main`) and a session is valid only on the
  listener that created it.
- **Shared with the main listener**: the account table and password generation
  (so L1 to L14 apply unchanged), the audit log, the idle and absolute session
  lifetimes, and the PBKDF2 slots (2 per process, so a second listener does not
  raise the CPU bound).
- **Separate from the main listener**: the rate limiter (its own tables and
  budgets, so an attacker who spends the budget for "admin" on one listener
  cannot throttle the other) and the recognised status: being inside
  `ADMIN_NETWORKS` is the gate to the listener, not a login privilege. A client
  is "recognised" here only with a valid device cookie of this origin.
- **CSRF**: `net/http.CrossOriginProtection` plus the per-session token. No
  `PUBLIC_URL` is configured for this listener, so the origin check compares
  `Origin` with the request's own `Host`.
- **Audit**: every record written while serving a request from this listener
  carries `"via":"admin-listener"` in its detail (a context value set by the
  listener's outermost middleware, not from client input; a different
  `via` set by the caller moves to `via_inner`).

## Attackers

| ID  | Attacker                                                              | Goal                                                   |
| --- | --------------------------------------------------------------------- | ------------------------------------------------------ |
| Y1  | LAN or internet client outside `ADMIN_NETWORKS`                       | learn that the listener exists, handshake, guess       |
| Y2  | client inside `ADMIN_NETWORKS` without credentials                    | guess the password                                     |
| Y3  | holder of a stolen cookie of one origin                               | use it on the other origin                             |
| Y4  | client that forges `X-Forwarded-*`, `X-Ingress-Path`, identity headers | change client address, scheme, base path; pose as IdP user |
| Y5  | web page in the admin's browser (other origin)                        | forge a login or an admin action                       |
| Y6  | on-path attacker in the admin VLAN                                    | impersonate the listener (self-signed certificate)     |
| Y7  | operator mistake                                                      | listener open to everyone, or silently off             |

## Claims and tests

| Claim | Statement                                                                                                                                                                                                                      | Tests |
| ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----- |
| X1    | A session is valid only on the listener that created it, in both directions. A rejected cookie value is not deleted by GET, logout or the rotation at login (a copied value cannot end the owner's session). Sessions that exist at migration 0003 belong to `main`. | `auth.TestSessionsAreBoundToTheirListener`, `auth.TestCopiedCookieCannotEndTheOtherListenersSession`, `store.TestSessionRemembersItsListener`, `store.TestMigration0003MarksExistingSessionsAsMainListener` |
| X2    | The admin listener has no OIDC route (404), `/login` leads to the local form, and the identity headers that authenticate on the main listener do not authenticate here. An admin `Service` cannot be built in OIDC or header mode. | `auth.TestAdminListenerHasNoOIDCOrHeaderAuth`, `auth.TestAdminListenerRequiresLocalMode`, `app.TestAdminListenerWorksWithTheIdPDown` |
| X3    | Forged `X-Forwarded-For/-Proto/-Host/-Prefix` and `X-Ingress-Path` change nothing: base path stays empty, scheme https, client address and rate-limit key are the TCP peer, a cross-origin POST is refused. | `auth.TestAdminListenerIgnoresForwardedHeaders`, `auth.TestAdminListenerRateLimitKeysOnThePeer`, `app.TestAdminListenerIgnoresForgedHeadersAndCrossOriginPosts` |
| X4    | Session, pre-session CSRF and device cookies of the admin origin use the `__Host-tasmofleet-admin*` names and are Secure, HttpOnly, `Path=/`, without `Domain`, even if a handler is reached without TLS context. | `auth.TestAdminCookiesAreHostOnlyAndSecure`, `auth.TestAdminSessionCookieIsSecureEvenWithoutTLSContext` |
| X5    | A device cookie (and so "recognised" status) earned on one listener is not honoured on the other: different names, listener-specific MAC keys.                                                                                  | `auth.TestDeviceCookiesDoNotCrossListeners` |
| X6    | The two listeners have separate login budgets: spending the budget for a user on one does not throttle the other. They share the 2 PBKDF2 slots of the process.                                                              | `auth.TestListenersHaveSeparateLoginBudgets`, `auth.TestListenersShareTheKDFSlots` |
| X7    | The account lifecycle holds on the second origin: a password change or disable ends admin-listener sessions at once and a disabled account cannot start one (L1, L2, L10).                                                     | `auth.TestAdminSessionDiesWithPasswordChange` |
| X8    | A connection from outside `ADMIN_NETWORKS` is closed in `Accept`: zero bytes, no TLS handshake, never seen by the HTTP server; IPv4-mapped peers match IPv4 prefixes; an allowed client behind a refused one is served.      | `httpx.TestNetworkFilterClosesOutsiderWithoutSendingBytes`, `httpx.TestNetworkFilterPassesInsiderAfterOutsider`, `httpx.TestNetworkFilterMatchesMappedIPv4`, `app.TestAdminListenerClosesConnectionsFromOutsideTheNetworks` |
| X9    | The start is refused when `ADMIN_LISTEN` has no port or a random one (ports are compared numerically: `:00`, `:https` and `:08080` are what they mean), the same port as `LISTEN`/`FIRMWARE_LISTEN`, no `ADMIN_NETWORKS`, a catch-all network (`/0`), an IPv4-mapped or zoned network, `LOCAL_LOGIN=false`, or half a certificate pair; `ADMIN_TLS_*` without `ADMIN_LISTEN` is an error. The shipped Unraid template loads with the listener on and restricted. | `config.TestAdminListenerValidation`, `config.TestUnraidTemplateEnablesARestrictedAdminListener` |
| X10   | Signing out on one listener clears that listener's cookie and does not touch the other listener's session.                                                                                                                     | `auth.TestAdminLogoutIsLocalToTheListener` |
| X11   | The self-signed certificate lives in its own directory, is reused across restarts and carries `ADMIN_TLS_SANS`. Every certificate that becomes the served one is logged with its fingerprint; `/about` shows the live one (also after renewal or reload); `tasmofleet tls fingerprint` prints the one on disk. | `httpx.TestSelfSignedSubdirKeepsListenersApart`, `httpx.TestCertChangesAreAnnouncedWithTheirFingerprint`, `app.TestAdminListenerCertificateIsStableAndAnnounced`, `cmd.TestTLSFingerprintPrintsTheAdminCertificate` |
| X12   | Audit records of requests from the admin listener (login and later actions) carry `via=admin-listener`; a caller-supplied `via` cannot hide it; the main listener's records are untouched.                                     | `app.TestAuditRecordsOnTheAdminListenerCarryVia`, `app.TestWithViaMergesAndOverrides` |
| X14   | Being inside `ADMIN_NETWORKS` does not make a client "recognised" on the admin listener (the `Service` ignores `AdminNetworks`); only a valid device cookie does. A cookieless host can neither spend the recognised per-user tier nor fill the recognised tables, so an admin with a device cookie still gets in; rotating usernames from one network is stopped by the network tier (20 per 15 min). | `auth.TestAdminNetworksAreNotRecognisedOnTheAdminListener`, `auth.TestUsernameRotationOnTheAdminListenerHitsTheNetworkTier` |
| X13   | With the IdP unreachable and no proxy, a local admin signs in on the admin listener and reaches the device list.                                                                                                              | `app.TestAdminListenerWorksWithTheIdPDown`, e2e `admin-listener.spec.ts` |

## Accepted residual risks

| ID  | Risk                                                                                                                                                                                                                                                  | Why accepted / mitigation |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------- |
| Z1  | Trust on first use: the certificate is self-signed, so the first connection depends on the operator comparing the fingerprint (log, `openssl` on the host).                                                                                           | An on-path attacker in the admin VLAN could otherwise be accepted. The runbook makes the comparison step one. `ADMIN_TLS_CERT_FILE` allows a certificate the browser trusts. The fingerprint changes when the certificate renews (yearly). |
| Z2  | A host inside `ADMIN_NETWORKS` is not special at login. Cookieless, it is a normal unrecognised client with its own budgets: 5 attempts per (network, user), 20 per 15 min per network, 30 per 15 min per user (then 1 per 10 s). A VLAN host that keeps the per-user tier busy slows a **first login from a new device** to one attempt per 10 s (R1 on this listener); it needs about two addresses to do so (one address is held to 20 attempts per 15 min by the network tier) and cannot be told apart from guessing. The two listeners do not share budgets, so this affects only the admin listener. | An admin who has signed in before has a device cookie (recognised: own tables, which no cookieless client can touch, X14). Otherwise the host CLI (`user passwd`, runbook section 2) is the way in. Guessing stays bounded as in C6. Rotating usernames is bounded by the network tier. The number of concurrent PBKDF2 runs is 2 per process. Unknown usernames are not pooled into one bucket: that would let an attacker tell existing from unknown names by the answer (the recognised tables only ever hold usernames that passed device-cookie validation, so rotation cannot fill them). |
| Z3  | Cookies are scoped by host, not by port, and `__Host-` only limits how a cookie is set. Every HTTPS origin on the same host name or IP receives the admin session cookie and can overwrite it (session swap, login CSRF). | The server-side listener binding stops replay between the main and the admin listener, but not a co-hosted HTTPS service. **Deployment rule: the container needs its own IP (Unraid `br0`).** There the only other ports are tasmofleet's own plain-HTTP ones, to which Secure cookies are not sent. Do not run it in bridge or host mode next to other HTTPS services. This cannot be detected from inside the container; the rule is in the runbook. |
| Z4  | Behind NAT (Docker bridge mode with a published port) every client appears with the gateway address; `ADMIN_NETWORKS` would then have to include it and the gate admits everyone who reaches the host port.                                         | Deployment rule (runbook): use a routable container address (Unraid `br0`), firewall the port to the admin VLAN as a second line. The log line "refused connections" shows what the gate sees. |
| Z5  | Audit `via` is set from the request context only. Work that continues after the request (for example OTA jobs a UI action started) records its events under its own context, without `via`.                                                          | The starting action itself is audited with `via`; job events refer to it. |
| Z6  | The SANs of an existing self-signed certificate are not re-checked: changing `ADMIN_TLS_SANS` has no effect until `<DATA_DIR>/tls/admin/` is deleted.                                                                                                 | Same behaviour as the main listener's certificate; runbook says how to regenerate. |
| Z7  | The admin listener serves the whole UI, not only account recovery. An admin who signs in there can do everything an admin can on the main listener (with the same step-up rules, L11/L12).                                                            | Narrowing it would make the emergency path less useful; every action is audited with `via`. |
