# Video operations (2026-11-12)

MiniMax H3 uses an asynchronous gateway bridge: `POST /v1/videos/generations`,
`GET /v1/videos/{id}`, and `GET /v1/videos/{id}/content`. The create response
contains a gateway `mmv_` task ID. The relay account needs an API key, an explicit
protocol, base URL, model mapping, and the `minimax_video` capability. Enable
media generation for the group. New H3 accounts default to LL2 at
`https://api.ll2.cn`, using upstream `POST /v2/video_generation` and
`GET /v2/query/video_generation/{provider_id}`. These supplier endpoints and
credentials belong in operator configuration; clients continue to use the
gateway routes and their gateway API keys.

`credentials.minimax_video_protocol` selects `ll2` or `getoken`; an absent or
empty value retains legacy Getoken behavior. Never infer protocol from the
hostname. Add a separate LL2 account and retain the old account until its tasks
finish and settle. Explicit protocol changes require a fresh key in the panel;
metadata-only edits preserve legacy settings. Custom proxy path prefixes are
preserved; LL2 URL construction removes only a trailing `/v1` or `/v2` before
adding the protocol path. Verify the proxy actually implements these endpoints.

LL2 H3 accepts 1–15 seconds, defaults to 5 seconds, accepts only 768p/768P, and
defaults to ratio 16:9. Other ratios are 21:9, 4:3, 1:1, 3:4, 9:16 and adaptive;
adaptive requires an image or video. Content requires exactly one text item of
1–7000 characters, plus up to 12 media items. First/last frames cannot mix with
reference images, videos or audio. References permit up to 9 images, 3 videos
and 3 audio clips within the combined limit. Simple prompt/image_url input and
content input are mutually exclusive. Media URLs must be public HTTPS; local
moderation covers text and images, not decoded video or audio. Parameter and
role validation happens before submission; supplier media-format limits still
require real-account acceptance.

Group settings `video_billing_mode` (`local_price`, `upstream_cost`, or
Seedance-only `usage_tokens`) and
`video_resolution_tiers` are persisted and included in API authentication
snapshots. The panel supports both create and edit. Explicit group settings
override environment defaults. 768p maps to the 720p pricing tier unless
overridden. Model prices override the group's resolution price. Independent
video multipliers and per-user group overrides use existing gateway semantics.
LL2 requires `local_price` with an explicitly configured seconds price and
rejects `upstream_cost` and `usage_tokens`. It does not report actual procurement cost.
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

This is not a universal monetary cap: token/upstream-cost amounts are estimates
until the provider returns final usage, and actual fees can exceed an estimate.
Account monetary quotas use provider cost (or token base cost), not the user's
local sale price. LL2 actual cost is unreported, so its account monetary usage
cannot be accumulated precisely from this API. LL2 accounts with enabled
monetary account quotas are rejected before submission; manage procurement
budgets in the supplier console. User balance, subscription and API-key quotas
still use the configured sale price. Any future purchase estimate must be
explicitly labelled as an estimate, not actual cost. Existing scheduler checks
of recorded account usage remain in place, without new durable account monetary
holds. Ordinary chat/image
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

This deployment uses RMB recharge and balance settlement: RMB 1 funds 1 unit
of site credit, displayed using the existing `$1` convention. No foreign
exchange conversion occurs. Local seconds and token prices use site credits;
`usd` field names and dollar symbols are compatibility names, not evidence
that LL2 charges US dollars. Reconcile procurement against supplier invoices.

When an RMB purchase baseline is entered as `p` site credits/second, an
effective multiplier of 1.2 sells at `p * 1.2` site credits/second. For an
illustrative RMB 0.05/second purchase baseline (subject to the actual supplier
invoice), configure 0.05 site credits/second and sell at 0.06 site credits/second.
A 5-second video deducts 0.30 site credits, against an illustrative procurement
baseline of RMB 0.25. This is 20% markup and 16.67% gross margin on sales,
assuming the baseline equals actual cost and ignoring other expenses. It is
not 20% gross margin. These are formula examples, not verified LL2 tariffs or
invoices. Unknown actual cost stays unreported; do not derive supplier cost or
profit from local prices. The admin video page separately accepts an estimated
purchase amount and an actual amount verified against supplier records. An
estimate never becomes actual cost automatically. Explicit zero prices and
multipliers remain valid.

LL2 settlement requires reported `usage.output_seconds` consistent with the
submitted duration. Missing or inconsistent seconds remain pending for further
polling/reconciliation; elapsed generation time and requested duration cannot
stand in for actual reported usage. Downloads may be available before billing
has completed. Legacy Getoken costs are recorded only when actually supplied.

Migration 343 adds a durable task snapshot and settlement timestamp. Migration
344 widens `video_tasks.upstream_cost_usd` and `billed_cost_usd` to NUMERIC(20,8),
matching eight-decimal ledger precision and preserving existing amounts.
Migration 345 adds `video_tasks.execution_state`, independent procurement
records and operation audit history. Apply all missing 340–345 migrations
before this version serves traffic. Token metadata, provider identity and
the immutable execution policy use the durable task snapshot; mutable retry
state is separate so ordinary task saves cannot reset its budget.
Successful
tasks use the existing transactional billing ledger, keyed by task ID, to
charge either balance or subscription and update configured quotas. Usage-log
and settlement-marker failures can be retried without charging twice. Failed
tasks are not charged. Completed charges cannot be erased by marking a task
failed in the admin panel.

Recovery starts through Wire with three independent loops for internal
settlement, Seedance and H3. Each loop has a one-minute ticker; a running batch
does not overlap itself, and slow requests can make an individual task's
interval longer. Each supplier batch rotates tasks by last poll time. A slow
supplier cannot block the other supplier or internal settlement; all loops
stop with the service context. Redis loss does not remove task history,
billing inputs or retry counters. The missing-ID 25-minute interruption rule
flags legacy Getoken and interrupted Seedance creation for reconciliation,
not automatic failure/refund or replay. Getoken's blocking create defaults to
a 20-minute request timeout. Known provider IDs are queried without creating
another task. Legacy rows without a billing snapshot are not charged
retroactively.

Account `extra.video_execution_policy` is validated and snapshotted at new
task admission. Request timeouts apply to both submit and status requests,
and are capped by the task's remaining automatic-processing lifetime.
Defaults are 30 seconds for LL2, 45 for Seedance and 1200 for Getoken; task
lifetimes default to 1200 seconds for H3 and 172800 for Seedance. Automatic
retry defaults to enabled, three retries and a minimum 30-second interval.
The retry count means additional attempts after the initial failed request;
healthy processing polls consume no budget and reset consecutive failures.
Missing settlement metadata counts as a failure while the completion itself
is preserved. Timeouts and exhausted budgets pause supplier I/O without
claiming cancellation or releasing an uncertain reservation.

Old tasks without an `execution_policy` snapshot keep their legacy execution
behavior. Changing the account does not retrofit them. An allowed explicit
administrator resume installs the provider's defaults for such a task, not
the account's current overrides. Resume resets the local deadline and budget
under provider locks and writes an audit entry atomically. It cannot safely
resume a missing-ID Getoken or Seedance submission, revive failed/deleted/
settled tasks, or extend LL2's original submission recovery window.

The finite retry budget governs supplier HTTP requests. Local idempotent
settlement, usage-log repair and settlement-marker recovery continue even
when supplier I/O is paused, if complete billing metadata already exists.
Missing usage cannot be guessed to permit settlement. Procurement edits do
not debit/refund users or rewrite usage logs and supplier-reported amounts.

LL2 stores the exact upstream request bytes, protocol, normalized base URL and
a SHA-256 identity of the API key and effective custom headers in the task
snapshot. It does not copy the raw key into the task snapshot. Submission
intent is saved before the outbound POST under a PostgreSQL advisory lock.
The upstream `Idempotency-Key` is the gateway task ID; configured header
overrides cannot replace this key or LL2 bearer authorization. On an ambiguous
create result, recovery reuses the same bytes, key and bound account identity
for at most 24 hours from admission, or an earlier stored expiry, and only
while the snapshotted task deadline and finite retry policy permit. The H3
default 20-minute task deadline can pause recovery much earlier. This is the
gateway's automatic recovery limit; the supplier has not committed to an
idempotency-record retention period, so this window is not a guaranteed
24-hour supplier idempotency TTL. Each attempt has a bounded request timeout.
A saved provider ID ends submission recovery; subsequent recovery only polls
that ID.

Protocol, URL or credential/header changes on the bound account stop recovery
and polling until the original configuration is restored. Do not mutate the
saved request or rotate its idempotency key to recover a task. An uncertain
submission keeps its durable reservation, including after its retry window
expires. Expiry only stops automatic creation attempts; it does not prove that
the provider cancelled or rejected the original request. Reconcile the original
task with supplier records before resolving the hold. A rejection following an
earlier ambiguous attempt also does not prove that the first request failed.
The 24-hour submission window does not expire known-ID polling; the separate
task deadline and retry budget still apply to those queries.

The billing ledger deduplicates by gateway task ID and API key. Repeated polls,
recovery and settlement retries cannot debit the main balance/subscription/API
key quotas twice. Do not manually mark an unresolved task failed or settled to
bypass a reservation; first establish its provider outcome and ledger state.

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

Seedance `execution_expires_after` is the provider execution lifetime: default
172800 seconds, allowed range 3600–259200. It is distinct from the account's
local automatic-processing deadline and retry budget. Known-ID queries use
the local policy; local timeout or prolonged lookup errors pause for review
without inventing provider cancellation or a terminal failure. A missing
provider ID after interrupted create is never replayed blindly.
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

See [VIDEO_API_GUIDE.md](VIDEO_API_GUIDE.md) for Chinese client instructions,
request/response fields, curl/Python/Node.js examples, billing, ownership and
retry behavior. Supplier setup and pricing belong in
[VIDEO_ADMIN_GUIDE.md](VIDEO_ADMIN_GUIDE.md). Local documentation edits do not
publish the website or deploy production; accept new behavior on the designated
test environment and record the exact commit and image used.

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
