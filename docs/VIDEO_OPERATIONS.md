# Video operations (0.5.2)

MiniMax H3 uses an asynchronous gateway bridge: `POST /v1/videos/generations`,
`GET /v1/videos/{id}`, and `GET /v1/videos/{id}/content`. The create response
contains a gateway `mmv_` task ID. The relay account needs an API key, a custom
base URL, model mapping, and the explicit `minimax_video` capability. Enable
media generation for the group. H3 defaults to 5 seconds at 768p; unsupported
H3 resolutions are rejected before upstream submission.

Group settings `video_billing_mode` (`local_price`, `upstream_cost`, or
Seedance-only `usage_tokens`) and
`video_resolution_tiers` are persisted and included in API authentication
snapshots. The panel supports both create and edit. Explicit group settings
override environment defaults. 768p maps to the 720p pricing tier unless
overridden. Model prices override the group's resolution price. Independent
video multipliers and per-user group overrides use existing gateway semantics.
The mode, price, payer and multiplier are fixed when the task is accepted.
Known local charges reserve balance, API key and subscription headroom before
submission. A PostgreSQL admission transaction locks the user/account and fresh
payer/key rows, checks active durable tasks and outstanding video holds, and
inserts the queued task atomically. Active user/account video concurrency and
these video holds survive Redis TTL expiry and application restarts. Completed
but unsettled tasks still hold headroom; a committed billing-ledger entry removes
that hold even if the later task settlement marker is delayed. Only the
completion ledger debits the payer. Redis reservations continue to coordinate
normal gateway traffic while the worker is alive.

This is not a universal dollar cap: token/upstream-cost amounts are estimates
until the provider returns final usage, and actual fees can exceed an estimate.
Account dollar quotas use provider cost (or token base cost), not the user's
local sale price; existing scheduler checks of recorded account usage remain
in place, without new durable account-dollar holds. Ordinary chat/image
admission does not read video-task holds from PostgreSQL; after Redis reservation
loss, ordinary traffic and videos do not have a shared durable reservation pool.
Use conservative account concurrency, adequate balance and provider-side limits,
and reconcile unknown-cost tasks before accepting unrestricted mixed traffic.

For Seedance `local_price`, the user can be billed correctly per second even
when the provider omits its USD cost. In that case supplier-cost statistics and
the account's dollar-quota usage cannot be accumulated precisely. Prefer
`usage_tokens` for BytePlus: account dollar usage can use the configured token
base tariff as an estimate, while actual provider cost remains marked as not
reported. This estimate is not a supplier invoice. User/platform-quota statistics
retain the official nontransactional cache update path: a crash immediately
after ledger commit may omit that secondary statistic. Transactional balance,
subscription and API-key ledger effects are unaffected.

With MiniMax-H3 configured at USD 0.10/second for 720p and multiplier 1, a
5-second 768p task costs the user USD 0.50. The provider's reported USD 0.375
cost is stored separately. Explicit zero prices/multipliers remain valid.

Migration 343 adds a durable task snapshot and settlement timestamp. Migration
344 widens `video_tasks.upstream_cost_usd` and `billed_cost_usd` to NUMERIC(20,8),
matching eight-decimal ledger precision and preserving existing amounts.
Apply all missing 340–344 migrations before this version serves traffic;
token metadata and provider state use the existing durable JSON snapshot.
Successful
tasks use the existing transactional billing ledger, keyed by task ID, to
charge either balance or subscription and update configured quotas. Usage-log
and settlement-marker failures can be retried without charging twice. Failed
tasks are not charged. Completed charges cannot be erased by marking a task
failed in the admin panel.

The recovery worker starts through Wire and checks every minute for incomplete
settlements. Redis loss does not remove task history or billing inputs. MiniMax
tasks still pending after the 20-minute upstream deadline plus 5-minute grace are
marked interrupted. Upstream create calls are never replayed automatically
after a crash. A process crash before a blocking provider returns may require
manual reconciliation with the provider. Legacy rows without a billing
snapshot are not charged retroactively.

Downloads require the owner's API key or panel session. Provider authorization
is sent only to the configured provider origin, including across redirects;
external CDN downloads do not receive that credential. Private destinations
and DNS rebinding are checked before connection.

Seedance uses Ark's `/api/v3/contents/generations/tasks` protocol. Its overseas
account preset uses `https://ark.ap-southeast.bytepluses.com/api/v3`, the provider
API key and explicit `seedance` capability. The group preset stores an OpenAI
compatible group with media permission and `usage_tokens` billing; it does not
route requests to OpenAI's official API. Model IDs or endpoint mappings must
match the provider account's actual entitlements.

New Seedance creation returns a durable gateway `sdv_` ID. Native status GET
and authenticated `/content` download use that ID. Background polling persists
completion and settles even when clients never poll. Both providers appear in
the unified user/admin task list and billing records, with actual token usage
and settlement state. Legacy non-`sdv_` Seedance IDs retain their older
owner-bound lookup path and Redis retention limitations.

The third group billing mode, `usage_tokens`, is Seedance-only and requires an
explicit flat token price card through existing model/channel pricing. The
bundled Seedance seconds-based metadata is not a token price card. Missing
output prices and tiered price cards reject admission; explicit configured
zero is valid. Actual provider tokens are used, never inferred from duration.
Seedance can instead explicitly use `local_price` with a priced resolution and
positive duration, or `upstream_cost` when the provider really reports dollars.
Missing usage/cost metadata remains unsettled and is queried again; missing
provider cost must not be interpreted as zero or used to fabricate profit.

Known-ID Seedance recovery follows `execution_expires_after` rather than the
MiniMax 25-minute interruption rule: default 172800 seconds, allowed range
3600–259200. A missing provider ID after interrupted create is never replayed
blindly. Prolonged provider failures beyond expiry can end local waiting with
an explicit reconciliation error; this does not confirm upstream cancellation.
Native Seedance DELETE hides terminal records only, returns 204 on success,
and returns 409 for running or completed-but-unsettled tasks. It does not cancel
billable generation or refund a completed charge.

Seedance `callback_url` and `return_last_frame:true` reject before submission
because raw callbacks and last-frame URLs bypass protected gateway task IDs
and output handling. `duration` and `frames` are mutually exclusive. Supported
content types are text, image_url, video_url and audio_url, including audio/video-
only requests if the chosen model supports them. Local moderation covers text
and images; the gateway does not decode audio/video for local moderation.
Individual models may support fewer input types. Current explicit resolution validation permits 480p, 720p
and 1080p. Input support and provider-specific limits require a real account
check, not just a successful mocked protocol test.

See [VIDEO_API_GUIDE.md](VIDEO_API_GUIDE.md) for complete Chinese client/admin
instructions, request/response fields, curl/Python/Node.js examples, billing,
ownership and retry behavior. Website publication does not deploy production;
new 0.5.2 functionality is first accepted on the designated test environment.

PR Wei-Shaw/sub2api#8622 is included: recognized Codex Desktop page-context
outputs are normalized before Responses tool validation. Unrelated malformed
tool outputs retain their existing validation behavior.

Deploy the immutable short-commit image tag and retain the previous image and
a database backup. Verify health, group-settings round trips, one complete
generation, DB balance/ledger/usage agreement, and both download endpoints.
Keep SSH passwords, API keys, and runtime command files outside the repository.
Before any app restart, drain current videos: HTTP graceful shutdown does not
wait for every provider generation goroutine. Preserve original JWT/TOTP keys,
runtime mounts and PostgreSQL/Redis versions. Verify the environment's actual
DATABASE_INITIALIZATION_ENABLED value before switching an image; it may be true
even though the project's default is false. Check the final candidate's entire
migration list, not just its version number, and do not rewrite old checksums.

Production task history does not permanently store provider video bytes.
Download expiry is independent from DB retention. Synthetic provider acceptance
must cover failure, missing/zero prices, client-free completion, owner checks,
downloads, repeated polls and restart recovery; it cannot verify real BytePlus
entitlement, quality, limits or invoices. Official image/moderation baseline
compatibility is documented in `UPSTREAM_BASELINE_REVIEW.md`.
