# D7 — OIDC account linking

**Current state:** SSO requires `email_verified` and matches on issuer+subject. A first SSO login whose email
belongs to an existing password account gets a 403 that says "ask an administrator to link it", but no
link endpoint or UI exists.
**Recommendation:** add a self-service "Link SSO" flow. The user must be signed in with their password,
then completes the OAuth round trip, and the identity binds to that session's user. Add admin unlink
alongside it. Never auto-link on email.
**Door type:** two-way door (the link is reversible through unlink, and the schema already exists).
Email-trust auto-linking (Option C) would be a one-way security-posture change and is rejected.

Evidence was verified on master `f99a302`. All paths are under `sidecar/`.

## 1. What the code does today

**Schema.**
- `sage.users` was created with `password TEXT NOT NULL` (`internal/schema/bootstrap.go:560-567`).
- The OAuth migration adds `oauth_provider`, drops `NOT NULL` on the password, and adds `oauth_issuer` and
  `oauth_subject` with a partial unique index on `(oauth_issuer, oauth_subject)`
  (`internal/schema/bootstrap.go:660-673`).
- An account is therefore password-only, SSO-only (`password IS NULL`) or, in principle, both. Nothing
  writes the "both" state today.

**Identity fetch.**
- OIDC userinfo requires `sub` and `email`, and requires `email_verified` to be `true` or `"true"`
  (`internal/auth/oauth_identity.go:77-107`).
- GitHub uses the numeric id plus a verified primary email (`:116-163`).
- The issuer is the configured `issuer_url`, or Google's fixed issuer (`:45-50`).

**Resolution (`FindOrCreateOAuthUser`, `internal/auth/oauth_users.go:19-51`).**
1. Reject unverified identities (`:29-31`).
2. Match on issuer+subject (`:35-40,53-69`).
3. `linkLegacyOAuthUser` binds issuer+subject only when all of these hold (`:73-85`):
   - `password IS NULL`;
   - `oauth_issuer IS NULL`;
   - the same `oauth_provider`.
4. Otherwise insert a new user with `ON CONFLICT DO NOTHING`. An email conflict with an unlinked account
   returns `ErrOAuthLinkRequired` (`:87-115`).

**Callback.**
- `ErrOAuthLinkRequired` maps to 403 JSON: "an account with this email already exists; ask an
  administrator to link it" (`internal/api/oauth_handlers.go:108-115,142-155`).
- The error text says the same (`internal/auth/oauth_identity.go:25-30`).

**OAuth state.**
- The state lives in memory: `states map[string]time.Time` (`internal/auth/oauth.go:32,125`).
- It is bound to the browser by an `oauth_state` cookie checked on the callback
  (`internal/api/oauth_handlers.go:43-54`; `internal/auth/oauth.go:145-160`).
- The state carries no user or intent.

**Admin user API.**
- Admin-only list, create, delete and role routes exist (`internal/api/router.go:393-412`). The rest of
  the user surface cannot express SSO linkage:
  - Create requires a password (`internal/api/auth_handlers.go:181-185`), so an admin cannot pre-create an
    SSO-only user.
  - The list response exposes no SSO linkage (`internal/api/auth_handlers.go:146-161`).
- No route reads or writes `oauth_issuer` or `oauth_subject`: `grep` finds them only in
  `internal/auth/oauth_users.go` and the schema.

**Defaults.**
- New SSO users get `oauth.default_role`, which defaults to `viewer` (`internal/config/config.go:467,966`;
  wired at `internal/api/router.go:386-390`).
- The config doc mentions "role mapping rule[s]" (`config.go:467`), but no role mapping exists.
- The bootstrap admin is `admin@pg-sage.local` (`cmd/pg_sage_sidecar/metadb.go:30`), so the default
  install never collides. Collisions come from password users that admins created with real corporate
  emails.

**Tests already in place:** `internal/auth/oidc_link_test.go:32,53,72,95,118,137,157` cover create,
subject match, refusal of password auto-link, subject change, unverified email, legacy link and concurrent
first login.

## 2. Concrete risk today

- **Blocked legitimate flow (the main issue).** An organisation that starts with password users and later
  enables SSO cannot move those users to SSO. The documented remedy has no endpoint behind it.
- **The only workaround loses data.** An admin deletes the user (`auth_handlers.go:220+`); sessions
  cascade (`bootstrap.go:573`). The user then logs in by SSO and is recreated with a new `id` and
  `default_role`, so an operator or admin is silently demoted to `viewer` until an admin re-grants the
  role. Audit history keyed by the old id no longer joins.
- **Security posture of today's refusal: good.** The 403 prevents the SURF-02 class, where an IdP-asserted
  email equal to an admin's email logs in as that admin (`reviews/2026-09-26/fixes-api-web.md:17`).
  Any fix must keep this property.
- **Adjacent (not in scope, noted):**
  - There is no domain or group allow-list. Any verified identity at the configured IdP gets an account
    with `default_role`.
  - If an operator sets `default_role: operator`, every IdP user becomes an operator.

## 3. Options

| Option | Mechanism | Proof of ownership | Door |
|---|---|---|---|
| **A. Admin binds issuer+subject** | `PUT /api/v1/users/{id}/oidc {issuer, subject}`, admin only. | The admin must obtain the subject out of band. This is error-prone: a wrong `sub` hands the account to someone else. | Two-way |
| **B. Self-service link from an authenticated session (recommended)** | A signed-in password user clicks "Link SSO". `GET /auth/oauth/authorize?intent=link` stores `{expires, link_user_id}` in the state. On the callback, bind issuer+subject to that user, provided that: the user has no identity yet, the identity is not bound elsewhere, and the verified email matches the account email (case-insensitive). | Both factors: the password session and the IdP login. | Two-way (admin unlink) |
| **C. Auto-link on verified email** (a config flag per trusted issuer) | Match an existing account by `email` when `email_verified`. | IdP email hygiene only. | **One-way in effect**: sessions minted under a wrong link cannot be recalled, and this reopens SURF-02 for any IdP with weak email verification. Rejected. |

**Recommendation: B**, plus two additions:
1. **Admin unlink** (`DELETE /api/v1/users/{id}/oidc`), which clears issuer/subject and deletes the user's
   sessions.
2. An optional **admin-issued one-time link grant** (`POST /api/v1/users/{id}/oidc-link-grant`, 15 min,
   single use) for users who no longer know their password. It runs the same callback path with
   `link_user_id` taken from the grant.

Do **not** null the password on link. Offer "disable password login" as a separate, explicit admin action.
Destroying the hash is irreversible, and today it would lock the user out if SSO breaks.

## 4. Implementation sketch

- **`internal/auth/oauth.go`**
  - Change `states map[string]time.Time` to `map[string]oauthState{Expires time.Time; LinkUserID int}`.
  - Add `AuthorizationURLForLink(userID int)`.
  - `ValidateState` returns the state (single use, as today).
- **`internal/auth/oauth_users.go`**: add `LinkOAuthIdentity(ctx, pool, userID, id Identity)`, which runs:
  ```sql
  UPDATE sage.users SET oauth_issuer=$1, oauth_subject=$2, oauth_provider=$3
  WHERE id=$4 AND oauth_issuer IS NULL AND lower(email)=lower($5)
  ```
  - If 0 rows are updated, return `ErrOAuthLinkConflict`.
  - A unique-index violation also maps to `ErrOAuthLinkConflict`.
  - Add `UnlinkOAuthIdentity`.
- **`internal/api/oauth_handlers.go`**
  - Authorize with `intent=link` requires an authenticated session and must not be a public route.
  - On the callback, a state with `LinkUserID` calls `LinkOAuthIdentity` and then redirects to
    `/#/profile?linked=1`. It must never create a user.
  - Map the new errors to 409. Log without the email (G6-B11 pattern, `oauth_handlers.go:140-141`).
- **`internal/api/router.go:393-412`**: add admin unlink and the optional link grant.
- **`internal/api/auth_handlers.go:146-161`**: add `sso_linked bool` and `sso_issuer` to the list response.
- **Web**
  - Add a "Link SSO" button on a profile/account view.
  - Add an unlink action and an SSO badge in `UsersPage`.
  - `LoginPage` should render the 403/409 message as a page, not raw JSON.
- **Schema:** none. The columns and unique index already exist (`bootstrap.go:660-673`).
- **Audit:** record link and unlink with the actor.

## 5. Test plan (write first; each must fail today)

**`internal/auth` (integration, real Postgres, same harness as `oidc_link_test.go:16-23`)**
- `TestLinkOAuthIdentity_BindsToPasswordUserPreservingIDAndRole`: after linking, the user id and role are
  unchanged and the password hash is still present.
- `TestLinkOAuthIdentity_RefusesIdentityBoundToAnotherUser`: returns `ErrOAuthLinkConflict`, and the other
  user is unchanged.
- `TestLinkOAuthIdentity_RefusesUserAlreadyLinked`.
- `TestLinkOAuthIdentity_RefusesEmailMismatch`.
- `TestFindOrCreateOAuthUser_AfterLinkReturnsSameUser`: a subsequent plain SSO login resolves to the linked
  id, not a new row.
- `TestUnlinkOAuthIdentity_ClearsIdentityAndSessions`.
- `TestLinkStateCannotBeReplayed`: the second callback with the same state fails.
- `TestPlainLoginStateNeverLinks`: a state without `LinkUserID` still returns `ErrOAuthLinkRequired` for a
  password email. This guards the existing `TestFindOrCreateOAuthUser_RefusesToAutoLinkPasswordAccount`.

**`internal/api`**
- `TestOAuthAuthorizeLinkIntentRequiresSession`: unauthenticated `intent=link` returns 401.
- `TestOAuthCallbackLinkIntentBindsSessionUser`: uses a stub IdP (pattern in `oidc_identity_test.go`).
- `TestAdminUnlinkRequiresAdmin`: an operator gets 403.
- `TestListUsersExposesSSOLinked`.

**Web (Vitest):** "Link SSO button calls the authorize endpoint with intent=link", and "UsersPage shows an
SSO badge and unlink for linked users".
