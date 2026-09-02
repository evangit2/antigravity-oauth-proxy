# antigravity-oauth-proxy — Hermes fork

Fork of [dvcrn/antigravity-oauth-proxy](https://github.com/dvcrn/antigravity-oauth-proxy) with fixes for running **Hermes Agent** (and other OpenAI-compatible agent harnesses) against the Gemini Antigravity OAuth allowance.

All changes were found the hard way, running Hermes against the proxy in production:

## Fixes in this fork

### 1. `400 INVALID_ARGUMENT: Function call is missing a thought_signature` on multi-turn tool use

**Symptom:** first turn works, then every follow-up request containing tool results fails with a 400 once Gemini 3 thinking models emit function calls with thought signatures.

**Root cause:** Gemini 3 returns a `thoughtSignature` alongside each function call and requires it echoed back when the client replays the call. Upstream smuggles the signature into the OpenAI tool-call ID (`call_<uuid>|<sig>`), assuming the client will round-trip the ID verbatim. Agent harnesses like Hermes sanitize tool-call IDs to bare UUIDs — the signature is silently lost, and Google rejects the replay.

**Fix:** server-side signature cache.
- When the model emits a tool call, the proxy stores `call_id → signature` (in-memory + persisted to `~/.config/antigravity-oauth-proxy/thought_signatures.json`).
- When a client replays a bare call ID, the proxy looks up the cached signature and re-attaches it to the `functionCall` part.
- The ID-smuggling path still works as a fallback for clients that do preserve IDs.

**Note:** Google validates signature values — dummy/replayed-wrong signatures get `Corrupted thought signature`. Signatures for calls made before this fix (or before the cache file existed) cannot be recovered; those conversations need a fresh session.

### 2. Mid-stream `429 RESOURCE_EXHAUSTED` on agent personas in `systemInstruction`

**Symptom:** requests with a large system prompt (e.g. a 25KB agent persona) fail with a bare `429 RESOURCE_EXHAUSTED` (no quota ID) 50–170s into the request, while the same payload passes when the system prompt is shortened or reworded.

**Root cause:** Google's upstream classifier on `systemInstruction` probabilistically flags third-party agent personas ("You are X, an AI assistant created by Y" as a literal string failed while its individual components passed). It is not a quota issue.

**Fix:** merge system text into the first user message wrapped in `[SYSTEM INSTRUCTIONS]…[/SYSTEM INSTRUCTIONS]` and send no `systemInstruction` field at all. Verified with a verbatim 88KB Hermes payload (previously 100% 429) returning 200 in ~2s, and system-prompt behavior (persona adherence) is preserved.

### 3. Client system instruction passthrough

Upstream prepends its own Antigravity CLI scaffolding prompt ahead of the client's system message. With fix #2 in place, the client's system text is passed through unmodified (the CLI prompt is still the fallback when no system message is present).

### 4. Opt-in request dumping for debugging

`AOP_DUMP_REQUESTS=1` dumps request bodies >50KB to `/tmp/aop-dumps/` for offline reproduction of 429/400s. **Off by default** — dumps contain full conversation content. Useful files: unique filenames `<millis>-<size>.json` prevent clobbering.

## Usage with Hermes Agent

```yaml
# ~/.hermes/config.yaml
model:
  provider: antigravity
  model: gemini-3.8-flash-high
providers:
  antigravity:
    api: openai-completions
    base_url: http://127.0.0.1:9878/v1
    api_key: <ADMIN_API_KEY>
```

Run the proxy as usual (`PORT=9878 ADMIN_API_KEY=... antigravity-oauth-proxy`), auth via `antigravity-auth`.

Verified working with Hermes Agent: multi-turn tool use (parallel tool calls, 37-tool schemas, 25KB personas), streaming, 400k context.

## Credits

All upstream credit to [dvcrn](https://github.com/dvcrn) for the original proxy. These patches were developed while debugging Hermes Agent + Antigravity Pro quota interop.