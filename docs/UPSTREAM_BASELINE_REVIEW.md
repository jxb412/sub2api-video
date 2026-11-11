# Upstream baseline and retained behavior

Reviewed on 2026-11-11 (Asia/Shanghai).

## Latest upstream merge

Video version 0.5.3 merges upstream main at
`530c5e3a4f9c73ca2b0881191d0ef8e0d82c5484`, including v0.2.21
(`10ba8f9b4dc2e6023d4a9879ab77b21841babd10`). The two subsequent
upstream commits only synchronize the upstream version and update sponsors.
Quya READMEs and the independent video version remain local.

Because the original video repository imported a source snapshot, the merge
first records its verified upstream ancestor `ee58c0013` without changing
the video tree, then performs a normal three-way merge of newer upstream
commits. Future upstream merges can use this ancestry directly.

The merge preserves all 277 locally changed files outside the upstream
intersection, including durable video tasks, billing/admission, recovery,
download authorization, account capabilities, groups and migrations 340–344.
Seedance and MiniMax model price entries retain their prior values; upstream
pricing entries retain the official updated values. PR #8622 normalization
appears once and still runs before Responses tool validation.

The only new database migration relative to the previous video release is
`249_add_usage_logs_api_key_cost_index_notx.sql`: a concurrent covering index
on `usage_logs(api_key_id, created_at) INCLUDE(actual_cost)`. No video schema
or settled billing records are rewritten. Applied migration checksums must
remain unchanged. The Build workflow now exercises the full Linux unit and
integration suites and frontend checks, rather than a video-only name filter.

Production `142.54.187.42` was separately upgraded to Quya 0.4.24-quya.1.
This video merge is validated and deployed only on the 154 test environment.

## Original imported baseline

The production-line merge `6862e1f57c8939eb7a482f1c05321e9176dd94df`
contains upstream `ee58c0013`, which is 14 commits ahead of the v0.2.20
release commit `dab829a4e9b7d1cc307189aea8b0319eb29e29d0` and includes that
release in its ancestry. The video repository imported a snapshot in
`4c66b47`; it does not retain the original repository's full commit ancestry.

The owner's merge policy is to use upstream implementations for GPT Image
2.6 and the long-conversation security fix, preserving unrelated Quya
features. Inspection of the imported video baseline confirms these files
match the merged upstream blobs exactly:

- `backend/internal/service/openai_cyber_session_block.go` and its tests.
- `backend/internal/service/content_moderation_input.go`.
- `backend/internal/service/openai_images.go`, `openai_images_direct.go`,
  and `openai_images_responses.go`.
- `frontend/src/composables/useModelWhitelist.ts`.

GPT Image 2.6 Flare/Sunburst use the official model declarations and image
handling, including complexity and supported native SVG/GIF/edit options.

Two distinct security mechanisms must not be conflated:

1. Content moderation's pre-hash cache stores normalized content hashes.
   The historical Quya commit `452d6f8b` added API-key/session-scoped hashes.
   Upstream still uses the content hash. Cache hits do not directly apply
   account-ban side effects or count as new ban-triggering violations.
2. Cyber session blocking previously rejected unrelated conversations once
   a source had been flagged and history exceeded 256 items. Upstream
   PR #8549 fixes this by matching recorded transcript prefixes at their
   exact lengths, with API-key-scoped session keys. The video baseline
   already includes this root-cause fix.

Retain the upstream implementations. The absence of the historical scoped
moderation helper is not evidence that the cyber fix was lost. Relevant
service and repository regression tests passed, including unrelated
256/257/1000-item histories, padded blocked-history continuation, GPT Image
2.6 native options and pricing, and complexity propagation.

The upstream main inspected was `e2c9812190dd71bb309c8ad020a1211da1e0d3b4`
(2026-11-10 UTC), 64 commits ahead of the merge's upstream parent. Its
security-audit worker wake-up change is separate from both mechanisms
above. That inspection does not imply all later upstream commits were
merged. Upstream PR #8622 is already imported separately as `aec476d`.

References:

- https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.20
- https://github.com/Wei-Shaw/sub2api/pull/8549
- https://github.com/Wei-Shaw/sub2api/issues/8519
- https://github.com/Wei-Shaw/sub2api/pull/8622
