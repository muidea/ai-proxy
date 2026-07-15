# Provider Profile Contract Register

Contract Version: PPC-2026-07-15.1

Status: active

Last Updated: 2026-07-15

## 1. Purpose and authority

This register is the versioned internal contract for each `providers.<route-owner>` profile. It supplements, but
never replaces, the runtime authority in `config.yaml`:

1. `config.yaml` determines the enabled route profile, protocol, exact model patterns and direct capabilities.
2. This register records why that declaration exists, the documentation source, the most recent live evidence and
   the role responsible for revalidation.
3. Public vendor documentation establishes protocol-level expectations only. A gateway/relay may expose a smaller
   subset for a particular route or model; the profile's live evidence is authoritative for that subset.
4. API keys, full base URLs with credentials, request bodies and raw upstream responses are not copied here.

The detailed result matrix is maintained in
[`provider-capability-audit-2026-07-15.md`](provider-capability-audit-2026-07-15.md).

## 2. Source and revalidation policy

| Field | Required rule |
| --- | --- |
| Source version | Use vendor version when published; otherwise record `unversioned` plus access date, or this contract version for internal gateways. |
| Evidence | Every declared direct capability needs a `PC-LIVE-*` task result; streaming is recorded separately where applicable. |
| Owner | `ai-proxy provider-integration owner` owns config and revalidation. `external-provider liaison` owns vendor/gateway escalation. |
| Cadence | Revalidate before a production release and at least every 7 calendar days while the profile is enabled. |
| Trigger | Revalidate immediately after a provider docs/release change, base URL/protocol/model pattern/capability change, credential rotation that changes behavior, or a live drift/5xx incident. |
| Change rule | Do not add a capability from documentation alone. Update config only after a successful direct probe, or remove it after explicit drift evidence. |

## 3. Profile records

### `api_test_abc`

| Item | Contract |
| --- | --- |
| Source class | Internal operational contract (no public vendor documentation registered) |
| Source version | PPC-2026-07-15.1 |
| Evidence | `PC-LIVE-20260715-DIRECT-02`, `PC-LIVE-20260715-STREAM-03`, `PC-GATEWAY-20260715-04` |
| Approved direct capabilities | `chat_completions`, `completions` |
| Exact evidence model | `gpt-5.5` |
| Exclusions | `embeddings` was removed after 404; no embedding model is currently published |
| Revalidation owner | ai-proxy provider-integration owner |
| Next revalidation | Before next release, within 7 days, or when the internal gateway contract changes |

### `deepseek`

| Item | Contract |
| --- | --- |
| Source class | Official vendor documentation + profile live evidence |
| Official source | [DeepSeek API Documentation](https://api-docs.deepseek.com/) |
| Source version | Unversioned public documentation, accessed 2026-07-15; profile contract PPC-2026-07-15.1 |
| Evidence | `PC-LIVE-20260715-DIRECT-02`, `PC-LIVE-20260715-STREAM-03` |
| Approved direct capabilities | `chat_completions` |
| Exact evidence model | `deepseek-v4-flash` |
| Exclusions | `responses` returned 404. `completions` requires the vendor beta API; it remains excluded until a dedicated, fully revalidated beta profile is introduced. |
| Revalidation owner | ai-proxy provider-integration owner; external-provider liaison for beta profile changes |
| Next revalidation | Before next release, within 7 days, or when DeepSeek API documentation/beta routing changes |

### `krill-ai`

| Item | Contract |
| --- | --- |
| Source class | Internal gateway contract + profile live evidence |
| Public source status | No usable published documentation is registered. `https://api.krill-ai.com/docs` returned Cloudflare 520 on 2026-07-15. |
| Source version | PPC-2026-07-15.1 |
| Evidence | `PC-LIVE-20260715-DIRECT-02`, `PC-LIVE-20260715-STREAM-03`, `PC-LIVE-20260715-KRILL-07` |
| Verified direct capabilities | `chat_completions`, `responses` |
| Exact evidence model | `grok-4.5` |
| Pending exception | `completions` remains declared but is not an approved capability conclusion: three probes returned Cloudflare 520. Do not advertise it as verified; revalidate when the gateway recovers. |
| Revalidation owner | external-provider liaison for 520 escalation; ai-proxy provider-integration owner for probe/config decision |
| Next revalidation | Immediately after the 520 incident clears; otherwise before release and within 7 days |

### `aiapi-minimax`

| Item | Contract |
| --- | --- |
| Source class | Public gateway protocol reference + internal relay contract + live evidence |
| Public source | [New API documentation](https://aiapi.bluetron.cn/docs) |
| Source version | New API web documentation is unversioned, accessed 2026-07-15; profile contract PPC-2026-07-15.1 |
| Evidence | `PC-LIVE-20260715-DIRECT-02`, `PC-LIVE-20260715-STREAM-03` |
| Approved direct capabilities | `chat_completions` |
| Exact evidence model | `MiniMax-M3` |
| Exclusions | `responses` was `not implemented`; `completions` and `embeddings` returned unsupported relay modes. |
| Revalidation owner | ai-proxy provider-integration owner; external-provider liaison for relay-mode changes |
| Next revalidation | Before release, within 7 days, or after New API relay/model mapping changes |

### `aiapi-deepseek`

| Item | Contract |
| --- | --- |
| Source class | Public gateway protocol reference + internal relay contract + live evidence |
| Public source | [New API documentation](https://aiapi.bluetron.cn/docs) |
| Source version | New API web documentation is unversioned, accessed 2026-07-15; profile contract PPC-2026-07-15.1 |
| Evidence | `PC-LIVE-20260715-DIRECT-02`, `PC-LIVE-20260715-STREAM-03` |
| Approved direct capabilities | `chat_completions` |
| Exact evidence model | `DeepSeek-V4-Flash` |
| Exclusions | `responses` and `embeddings` were `not implemented`; `completions` returned 404. |
| Revalidation owner | ai-proxy provider-integration owner; external-provider liaison for relay-mode changes |
| Next revalidation | Before release, within 7 days, or after New API relay/model mapping changes |

## 4. Change procedure

1. Update the relevant record's source version, access date and owner acknowledgement.
2. Run the matching non-stream direct probe; run streaming for a successful streaming-capable endpoint.
3. Append the task ID, status and safe summary to the capability audit.
4. Update `config.yaml` only if the result changes the approved capability set; then deploy and verify `/v1/models` and
   local typed rejection for removed capabilities.
5. Keep an unresolved gateway failure as `environment_undetermined`; do not silently add, remove or claim support.
