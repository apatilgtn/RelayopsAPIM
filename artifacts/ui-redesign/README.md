# RelayOps UI redesign — 5 October 2026

Implemented in the embedded product assets. The Go deployment model, routes, authentication storage and API contracts remain in place. Existing working-tree backend changes were retained.

## Delivered

- Shared locally bundled Inter, Lucide SVG icons, green/neutral design tokens and consistent controls, tables, badges and dialogs.
- Console navigation grouped into Observe, Manage, Release and Administration; concise page headers, permission-aware controls, secondary row menus and scrollable tables.
- Release workflow guidance, labelled revision selectors, correctly rendered backend comparison results, configuration field diffs and reviewed plan hashes, with confirmations before consequential actions.
- Compact developer catalog with search, categories, sorting, cards/list views and dismissible quickstart. Request examples depend on the selected API. Existing catalog entries remain intact.
- Clear email/password, administrator-token and request-access modes, responsive layouts, visible focus, dialog focus trapping/restoration and reduced-motion support.

## Verification

| Check | Result |
| --- | --- |
| JavaScript syntax: app, portal, authentication and shared helpers | Passed |
| Authored HTML/JS/CSS emoji scan | Passed |
| Embedded public CSS, font, helper and SVG route tests | Passed in the final full Go runs |
| Locally loaded Inter | Confirmed through the browser font set |
| Eleven console views with populated data | Reviewed |
| Empty console views and database-unavailable console views | Reviewed; error messages remain visible with retry controls |
| Catalog search, no-match state, list/cards switch, reference and workbench | Reviewed; isolated GET request returned HTTP 200 |
| Signup required-field validation and administrator-token sign-in | Reviewed against the current isolated backend |
| Configuration planning/apply to canary, revision comparison, approval and promotion dialogs | Exercised using disposable database fixtures; approval/promotion confirmations were cancelled |
| Dialog keyboard trap and focus restoration | Passed; closing restores the opening control |
| Responsive widths | 1440, 1024 and 390 CSS pixels checked; all eleven console views checked at 390; no page-level horizontal overflow observed |
| 200% zoom | Browser shortcut unsupported by the automation surface; 720px reflow checked for API and release views. Actual browser zoom still needs manual confirmation |
| Product executable | Rebuilt successfully at `bin/relayops-ui.exe`; running on port 9090 |

The final `go test ./...` runs passed both without integration enabled and with `RELAYOPS_TEST_DATABASE_URL` set to the local PostgreSQL server. Database tests provisioned disposable databases. The earlier declarative-plan constraint failure and concurrent tenant-related compilation failures were resolved by the current backend changes before this restart; their tests now pass.

The earlier Go run without database integration passed. The ordinary test run skips the opt-in browser review server. To repeat manual UI review, set `RELAYOPS_UI_REVIEW=1` and run `go test ./internal/admin -run TestUIReviewServer -v -timeout 30m`. It provisions disposable databases and serves populated, empty and unavailable-database fixtures on localhost ports 9196, 9198 and 9199.

The current backend changes, including tenant-scoped declarative operations, were retained in the final build.

Manual review sampled validation, empty, error and database-unavailable states. Every possible loading/error transition and a full accessibility audit were not exhaustively exercised. Pending-approval account lifecycle coverage remains in the existing backend tests; no real account passwords or approvals were changed for this review.

## Screenshots

Before screenshots use the earlier running build. After console screenshots use disposable fixtures; the catalog uses the existing product catalog read-only.

| Screen | Before | After |
| --- | --- | --- |
| Catalog | [Before](before-catalog.jpg) | [After](after-catalog.jpg) |
| Sign in | [Before](before-login.jpg) | [After](after-login.jpg) |
| API list | [Before](before-api-list.jpg) | [After](after-api-list.jpg) |
| Releases & fleet | [Before](before-releases.jpg) | [After](after-releases.jpg) |

Additional captures: [mobile sign in](after-login-mobile.jpg), [mobile catalog](after-catalog-mobile.jpg), [request access](after-signup.jpg).

## Running the product

At the user's explicit request, the existing service was replaced with the current build on port 9090, retaining its existing database and configuration. The data plane remains on port 8080. A PostgreSQL custom-format backup and the previous executable are saved under `bin/backups/`.

Before/after read-only checks confirmed the same 47 API IDs, 31 consumer IDs, 8 plan IDs, 22 subscription IDs and 49 administrator-user IDs. Startup's existing drift-adoption controller generated revision 173 after recognizing the current database configuration. No release was manually promoted or rolled back.

Administrator-token login previously returned a session that subsequent requests rejected (401); on the current app the session is accepted (200). Overview, fleet, logs, audit and analytics requests succeeded using that session. Exporting and planning the existing configuration returned a valid plan with no changes. Invalid administrator tokens remain rejected (401). The health endpoint reports the database healthy and gateway ready with 38 loaded routes. All eleven console views and the public catalog were reviewed on the restarted application without browser console errors.

Open `http://127.0.0.1:9090/login` or `http://127.0.0.1:9090/portal`. The separate port-9196 preview used disposable accounts and was stopped. Existing email/password credentials were not reset; password sign-in cannot be verified without the user's password. [Running app screenshot](running-9090.jpg).
