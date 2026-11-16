# Keycloak setup for tasmofleet

Example deployment for Keycloak. The menu paths and names below are those of the Keycloak admin
console, checked against Keycloak 26.8.2; other versions move some switches (for example older
versions have "Proof Key for Code Exchange Code Challenge Method" under "Advanced"). tasmofleet is a
confidential OIDC client (authorization code flow with PKCE); roles come from the `groups` claim.
For Authentik see [deploy/authentik](../authentik/README.md).

## Groups

In the realm, open Groups, "Create group", and create the two groups. Then add each user under
Users, the user, tab "Groups", "Join Group".

| Group               | tasmofleet role | Variable                                      |
| ------------------- | --------------- | --------------------------------------------- |
| `Tasmofleet Admins` | admin           | `TASMOFLEET_ADMIN_GROUPS=Tasmofleet Admins`   |
| `Tasmofleet Users`  | operator        | `TASMOFLEET_OPERATOR_GROUPS=Tasmofleet Users` |

A user in none of the groups gets a 403 page and no session. Group names are compared exactly,
and Keycloak writes a group as its path (`/Tasmofleet Admins`) unless the mapper below turns that
off. Use plain names as in the table, or the paths with a leading slash in the variables; do not
mix them.

## Client

Open Clients, "Create client".

| Setting                         | Value                                                                                                                          |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| Client type                     | OpenID Connect                                                                                                                 |
| Client ID                       | `tasmofleet` (any name; it goes into `TASMOFLEET_OIDC_CLIENT_ID`)                                                              |
| Client authentication           | On (a confidential client)                                                                                                     |
| Authentication flow             | Standard flow on; Direct access grants off, everything else off                                                                |
| Require PKCE, PKCE Method       | On, `S256` (tasmofleet always sends `S256`)                                                                                    |
| Valid redirect URIs             | `https://tasmofleet.example.com/oidc/callback` (`TASMOFLEET_PUBLIC_URL`, the base path, `/oidc/callback`)                                     |
| Valid post logout redirect URIs | `https://tasmofleet.example.com/` (the value of `TASMOFLEET_OIDC_POST_LOGOUT_URL`; by default `TASMOFLEET_PUBLIC_URL` plus the base path) |

The client secret is on the tab "Credentials". The realm's default signing key is RS256, which
tasmofleet needs; leave it as it is.

Without the post logout redirect URI, "Sign out" in tasmofleet ends the Keycloak session and then
stops at a Keycloak page that says "Invalid redirect uri".

## Groups claim

Keycloak puts no group list into a token by itself. Add it to the client's own scope: Clients,
`tasmofleet`, tab "Client scopes", `tasmofleet-dedicated`, "Configure a new mapper", "Group
Membership":

| Setting          | Value                                                                             |
| ---------------- | --------------------------------------------------------------------------------- |
| Name             | `groups`                                                                          |
| Token Claim Name | `groups` (the same as `TASMOFLEET_OIDC_GROUPS_CLAIM`, which defaults to `groups`) |
| Full group path  | Off (plain names; with On the claim reads `/Tasmofleet Admins`)                   |
| Add to ID token  | On                                                                                |

"Add to ID token" is on by default in a new mapper; keep it. tasmofleet reads the groups from the
ID token only, at sign-in and again on every re-check from the ID token of the refresh response.
With it off, every user gets the 403 page. Keycloak adds the claim to the refreshed ID token
too, so a change of group membership reaches tasmofleet at the next re-check
(`TASMOFLEET_OIDC_RECHECK`, 15 minutes): a promotion applies, and a user who loses all groups is
signed out there.

Do not use the built-in client scope `microprofile-jwt` for this. Its `groups` claim carries the
user's realm roles, not groups. The scope is optional and not requested unless you list it in
`TASMOFLEET_OIDC_SCOPES`.

## Refresh token and offline access

tasmofleet also requests `offline_access` to get a refresh token. With Keycloak's defaults this
works: `offline_access` is an optional client scope of every new client, and the realm role
`offline_access` is part of `default-roles-<realm>`. Two ways to break it, both seen as an error
page "Sign-in failed" in tasmofleet:

- the scope `offline_access` is removed from the client: Keycloak answers `invalid_scope`;
- a user without the realm role `offline_access`: Keycloak answers "Offline tokens not allowed for
  the user or client".

Sign-out in tasmofleet ends the Keycloak browser session. The offline session behind the refresh
token stays in Keycloak until its idle time runs out ("Offline Session Idle", 30 days by default;
see Sessions in the realm and in the client). It cannot sign anyone in to tasmofleet again, because
tasmofleet needs the browser session for that.

## Step-up for user changes

An administrator who signed in through Keycloak confirms changes to users by signing in again.
tasmofleet sends `prompt=login` and `max_age=0` and needs `auth_time` in the ID token. Keycloak
does both with its default settings: it asks for the credentials again and adds `auth_time`.

## tasmofleet side

Set `TASMOFLEET_PUBLIC_URL` (container configuration; only the environment or the env file can
set it), then fill in the rest under System, Settings, tab "Sign-in" (admin, needs your
password), and press "Test discovery". The values are stored in the database; they take effect
after a restart, and local accounts keep working if anything is wrong.

Alternatively set them in the container configuration (the page then shows them read-only,
with the badge "Environment"):

```
TASMOFLEET_AUTH_MODE=oidc
TASMOFLEET_PUBLIC_URL=https://tasmofleet.example.com
TASMOFLEET_OIDC_ISSUER=https://keycloak.example.com/realms/tasmofleet
TASMOFLEET_OIDC_CLIENT_ID=tasmofleet
TASMOFLEET_OIDC_CLIENT_SECRET_FILE=/data/oidc-client-secret
TASMOFLEET_ADMIN_GROUPS=Tasmofleet Admins
TASMOFLEET_OPERATOR_GROUPS=Tasmofleet Users
```

The issuer is the realm URL, `https://<keycloak host>/realms/<realm>`, with no slash at the end
(Authentik's ends with one). It must match the `issuer` of
`<issuer>/.well-known/openid-configuration` character for character; with a slash added, tasmofleet
logs "issuer URL provided to client ... did not match" and sign-in stays unavailable. Check the
discovery document in a browser and copy the value from it.

"Test discovery" refuses an issuer on the loopback interface ("no usable OIDC provider"), so a
test Keycloak on `127.0.0.1` shows that message on the page while sign-in itself works; a private
network address passes.

Every variable in the container environment needs the `TASMOFLEET_` prefix: a name without it
is ignored, without any message. The same keys without the prefix are valid only as lines in
the env file `/data/tasmofleet.env`. See [configuration](../../docs/configuration.md).

Each setting has one source, the page or the container configuration, never both. A value from
the environment wins on every start, is never copied into the database, and a save from the page
that names it is refused. The issuer and the client secret lock together: set
`TASMOFLEET_OIDC_ISSUER` and the client secret in the same place (both as variables, or both on
the page). With only the secret in the environment, the issuer saved on the page is ignored
and sign-in falls back to local accounts at the next start. A client secret entered on the page
is bound to the issuer's scheme, host and port; after the issuer moves to another host, enter it
again. After changing a variable, re-create the container (`docker compose up -d`);
a restart keeps the old environment.

Never put the client secret into a file in this repository.

## Header mode

Not covered here: this guide was only checked with `TASMOFLEET_AUTH_MODE=oidc`.

## Logo and login theme (optional)

tasmofleet neither sends nor reads a logo, an icon or any other client metadata, and its web UI
always shows its own icon. Nothing here changes tasmofleet.

What Keycloak does with a client logo: the client attribute `logoUri` (the Admin REST API sets it;
the admin console of 26.8 has no field for it) is shown only on Keycloak's consent screen, and
only when "Consent required" and "Display client on screen" are on for the client. The sign-in page
itself shows no client logo.

To make the Keycloak sign-in page look like tasmofleet, use a login theme. A community theme
exists: [tasmofleet-keycloak-theme](https://github.com/Noschvie/tasmofleet-keycloak-theme) by
@Noschvie (light and dark). It is not part of tasmofleet, not maintained here and not tested
against it; its own repository has the installation steps. Select it in the realm, or for this
client only under Clients, `tasmofleet`, "Login settings", "Login theme".
