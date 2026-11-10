# Video operations (0.5.1)

MiniMax H3 uses an asynchronous gateway bridge: `POST /v1/videos/generations`,
`GET /v1/videos/{id}`, and `GET /v1/videos/{id}/content`. The create response
contains a gateway `mmv_` task ID. The relay account needs an API key, a custom
base URL, model mapping, and the explicit `minimax_video` capability. Enable
media generation for the group. H3 defaults to 5 seconds at 768p; unsupported
H3 resolutions are rejected before upstream submission.

Group settings `video_billing_mode` (`local_price` or `upstream_cost`) and
`video_resolution_tiers` are persisted and included in API authentication
snapshots. The panel supports both create and edit. Explicit group settings
override environment defaults. 768p maps to the 720p pricing tier unless
overridden. Model prices override the group's resolution price. Independent
video multipliers and per-user group overrides use existing gateway semantics.
The mode, price, payer and multiplier are fixed when the task is accepted.
Known local charges reserve balance, API key and subscription headroom before
submission. Reservations renew while the worker runs and release on completion;
only the completion ledger debits the payer.

With MiniMax-H3 configured at USD 0.10/second for 720p and multiplier 1, a
5-second 768p task costs the user USD 0.50. The provider's reported USD 0.375
cost is stored separately. Explicit zero prices/multipliers remain valid.

Migration 343 adds a durable task snapshot and settlement timestamp. Successful
tasks use the existing transactional billing ledger, keyed by task ID, to
charge either balance or subscription and update configured quotas. Usage-log
and settlement-marker failures can be retried without charging twice. Failed
tasks are not charged. Completed charges cannot be erased by marking a task
failed in the admin panel.

The recovery worker starts through Wire and checks every minute for incomplete
settlements. Redis loss does not remove task history or billing inputs. Tasks
still pending after the 20-minute upstream deadline plus 5-minute grace are
marked interrupted. Upstream create calls are never replayed automatically
after a crash. A process crash before a blocking provider returns may require
manual reconciliation with the provider. Legacy rows without a billing
snapshot are not charged retroactively.

Downloads require the owner's API key or panel session. Provider authorization
is sent only to the configured provider origin, including across redirects;
external CDN downloads do not receive that credential. Private destinations
and DNS rebinding are checked before connection.

Seedance uses its existing Ark task protocol at `/api/v3/contents/generations/tasks`;
BytePlus international endpoints use the same protocol and require a separate
configured account with the `seedance` capability. Seedance uses its existing
token usage billing. The new video task panel currently lists MiniMax tasks.
Protocol regression tests do not replace live testing with a BytePlus account.

PR Wei-Shaw/sub2api#8622 is included: recognized Codex Desktop page-context
outputs are normalized before Responses tool validation. Unrelated malformed
tool outputs retain their existing validation behavior.

Deploy the immutable short-commit image tag and retain the previous image and
a database backup. Verify health, group-settings round trips, one complete
generation, DB balance/ledger/usage agreement, and both download endpoints.
Keep SSH passwords, API keys, and runtime command files outside the repository.
