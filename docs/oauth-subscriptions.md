# OAuth logins & using ChatGPT / Claude subscriptions

Zero supports two distinct things people mean by "log in with OAuth":

1. **OAuth login for a provider/gateway** that issues a *standard* bearer token —
   fully built in (`zero auth login …`). Zero attaches the token to model calls
   automatically.
2. **Using a ChatGPT or Claude *subscription*** (Plus / Pro / Max) instead of a
   pay-per-token API key — only possible through a **local proxy**, for the
   reasons documented below. Zero ships a convenience preset and this recipe.

---

## 1. OAuth login for a provider or gateway (built in)

For any OAuth 2.0 / OIDC provider that returns a normal access token usable as
`Authorization: Bearer …` on its API, configure it with `ZERO_OAUTH_<NAME>_*`
env vars and log in:

```sh
export ZERO_OAUTH_ACME_CLIENT_ID=…
export ZERO_OAUTH_ACME_AUTHORIZE_URL=https://acme.example/oauth/authorize
export ZERO_OAUTH_ACME_TOKEN_URL=https://acme.example/oauth/token
export ZERO_OAUTH_ACME_SCOPES="openid profile"
zero auth login acme            # browser (loopback); --device for headless
zero auth status
```

When a login exists for a provider, the **OpenAI and Anthropic** providers send
`Authorization: Bearer <fresh-token>` (auto-refreshed; one refresh-and-retry on a
`401`) instead of the API key. With no login they use the API key exactly as
before. Tokens are stored 0600 (or the OS keyring with
`ZERO_OAUTH_STORAGE=keyring`) and never logged. See `zero auth --help`.

Provider OAuth login validates the persisted user configuration before opening
authorization and revalidates it immediately before replacing a stored token.
This applies to CLI login, the TUI/setup wizard, and device-code completion. If
the configuration is invalid at either check, Zero aborts without overwriting
the previous OAuth credential. If the error identifies one legacy unnamed
provider profile, repair it with `zero providers repair-config`.

### In the setup wizard (`/provider`)

Running `/provider` opens a **"How do you want to connect?"** chooser:

```text
❯ Sign in with OAuth                 One-click browser login (OpenRouter, xAI, ChatGPT, Hugging Face)
  Paste an API key / browse providers  Any of 20+ providers, local, or a proxy
```

Pick **Sign in with OAuth** → the list of providers that do real OAuth → choose one:

```text
❯ OpenRouter      browser sign-in · creates a key
  xAI (Grok)      browser or device code
  ChatGPT         browser (Codex backend, ChatGPT Plus/Pro)
  Hugging Face    browser or device code
```

- **OpenRouter / xAI / ChatGPT / Hugging Face** are real OAuth: your browser
  opens to approve → done (no key to paste). OpenRouter mints a key; xAI /
  ChatGPT / Hugging Face store a refreshable bearer. Hugging Face requires a
  one-time OAuth-app registration (no secret needed for "public" apps); the
  preset pre-fills scopes, endpoints, and the OIDC issuer. The same chooser
  appears in first-run onboarding. (xAI uses an opt-in preset — set
  `ZERO_OAUTH_ALLOW_PRESETS=1` or your own `ZERO_OAUTH_XAI_*`; see below.)
- **Device code (headless / SSH):** for a provider that supports it (xAI,
  Hugging Face), press **d** on the list to get a code to enter on another
  device instead of opening a browser. On an SSH session or headless Linux box
  (no `DISPLAY`) device code is used automatically; set `ZERO_OAUTH_DEVICE=1`
  to force it anywhere. The CLI equivalent is
  `zero auth login <name> --device`.
- **ChatGPT / Claude are intentionally not in this list for the proxy path** —
  use the dedicated `chatgpt-proxy` / `custom-anthropic-compatible` preset
  (see §2) for subscription-via-proxy. ChatGPT *is* a first-class OAuth
  provider in this version (routes to the Codex backend) — see "Built-in OAuth
  providers" below.

### Built-in OAuth providers

- **OpenRouter (no env needed)** — `zero auth openrouter` opens a browser, you
  approve, and it **mints an OpenRouter API key** (public PKCE flow, no client_id).
  In the interactive setup wizard, pick **OpenRouter** and press **ctrl+o** at the
  key step to do the same inline ("Log in with OAuth"). The minted key is saved to
  the provider profile and used normally.
- **xAI (Grok) — opt-in preset** — xAI's flow needs an OAuth `client_id`. Zero
  ships a built-in preset for the public Grok-CLI client, but to keep third-party
  client identities out of the default credential path it is **off by default**.
  Enable it with `export ZERO_OAUTH_ALLOW_PRESETS=1`, then `zero auth login xai`
  (browser, or `--device` for headless) works one-click; the token is used directly
  on `api.x.ai/v1`. Without the opt-in, set `ZERO_OAUTH_XAI_CLIENT_ID` (and
  endpoints, or an issuer) yourself via `ZERO_OAUTH_XAI_*`. Either way the preset is
  fully overridable by `ZERO_OAUTH_XAI_*` (env wins), and it requires a
  SuperGrok / X Premium+ subscription; the client_id is an undocumented public
  Grok-CLI client that may change without notice.
- **ChatGPT (Codex) — opt-in preset** — `zero auth chatgpt` opens a browser, you
  approve with your ChatGPT Plus/Pro/Business/Enterprise account, and the bearer is
  stored. The bearer routes to `https://chatgpt.com/backend-api/codex/responses`
  (the same endpoint the openai/codex CLI uses), with `originator: codex_cli_rs` and
  the `chatgpt-account-id` claim injected as headers on every request. The
  `chatgpt-account-id` is extracted from the OIDC ID token and stored alongside the
  bearer; if the claim is missing (older ChatGPT accounts, or a rotated
  authorization server), the Codex backend will 401 and `zero auth status chatgpt`
  will show the warning. Like xAI, the preset uses the publicly-shipped Codex CLI
  client identity (`app_EMoamEEZ73f0CkXaXp7hrann`) and is opt-in via
  `ZERO_OAUTH_ALLOW_PRESETS=1`. As of mid-2026 the Codex backend is
  Cloudflare-gated: requests from a non-Codex client can still be challenged, and
  the `chatgpt-proxy` route in §2 is the conservative fallback.
- **Hugging Face — opt-in preset, BYO client_id** — `zero auth login huggingface`
  (or `--device` for headless) opens a Hugging Face OAuth flow. The bearer works on
  the OpenAI-compatible router at `https://router.huggingface.co/v1` for hundreds
  of OSS models (Llama, Qwen, DeepSeek, Mistral, etc.). HF does not ship a
  globally-known client_id, so the preset ships endpoints + scopes + the OIDC
  issuer pre-filled; you must register a "public" OAuth app (no secret) at
  <https://huggingface.co/settings/applications/new> and set the resulting
  `client_id` via `ZERO_OAUTH_HUGGINGFACE_CLIENT_ID`. Enable the preset with
  `ZERO_OAUTH_ALLOW_PRESETS=1` (or omit it — the BYO client_id path uses
  `client_credentials = none` and doesn't need the opt-in). Free tier has strict
  rate limits; Pro removes them.

Any field of a preset is overridable via `ZERO_OAUTH_<NAME>_*`. For a fully custom
OAuth/OIDC provider, set those env vars (see `zero auth --help`) and
`zero auth login <name>`.

---

## 2. ChatGPT / Claude subscriptions — why a proxy is required

We researched this carefully. As of mid-2026, a **subscription** OAuth token does
**not** work as a drop-in bearer against the standard APIs:

- **OpenAI (ChatGPT):** a "Sign in with ChatGPT" token only works against
  ChatGPT's own backend (`chatgpt.com/backend-api/codex/responses`, the Responses
  API), **not** `api.openai.com`. That backend is **Cloudflare bot-protected** —
  non-browser / headless clients get `cf-mitigated: challenge` → `403`. It also
  requires mimicking the official Codex client (originator + account-id header).
  **First-class path (this version):** `zero auth chatgpt` does exactly that
  mimicking (`originator: codex_cli_rs`, `chatgpt-account-id: <claim>`) and
  routes requests to the Codex backend, no proxy required — see §1. The
  `chatgpt-proxy` route below is the conservative fallback when Cloudflare
  challenges become an issue, and is the only path that works without a
  browser-based ChatGPT OAuth login.
- **Anthropic (Claude):** the Messages API **rejects** subscription OAuth tokens
  for third-party use unless the request spoofs the Claude Code identity
  (`anthropic-beta: oauth-2025-04-20`, `claude-cli` UA, and a verbatim
  *"You are Claude Code…"* system prompt) — and **even then** tool-using requests
  on Max plans are routed to a disabled billing lane and `400`. Anthropic's policy
  **prohibits** subscription-token use outside Claude Code / claude.ai, and the
  timeline hardened through 2026: a **Feb 19 2026** docs update spelled out that
  Free/Pro/Max OAuth tokens may not be used in third-party tools or the Agent SDK,
  then on **April 4 2026** enforcement landed and subscription OAuth tokens
  **stopped working in third-party harnesses** (starting with OpenClaw, then the
  rest). As of mid-2026 the only supported ways to drive Claude from a third-party
  tool are a standard **API key** or pay-as-you-go **"Extra Usage"** billing —
  both per-token, not the flat subscription. The request to allow subscription use
  (claude-code #37205) was closed *"not planned."*

So Zero does **not** call those backends directly or spoof those clients — that
would be fragile, account-risky, and (for Anthropic) against the vendor's terms.
The robust, supported pattern is a **local proxy** that holds your subscription
session and exposes a clean OpenAI- or Anthropic-compatible endpoint on
`127.0.0.1`. The proxy absorbs the Cloudflare / client-spoofing surface; Zero
just points at it.

### ChatGPT via a local proxy

Run a local ChatGPT OAuth proxy that exposes an OpenAI-compatible endpoint (these
typically listen on `127.0.0.1:10531/v1`). Then use the built-in **`chatgpt-proxy`**
preset (no API key — the proxy authenticates):

```jsonc
// ~/.config/zero/config.json (or ./.zero/config.json)
{
  "activeProvider": "chatgpt",
  "providers": [
    {
      "name": "chatgpt",
      "catalogID": "chatgpt-proxy",     // OpenAI-compatible, local, no key
      "baseURL": "http://localhost:10531/v1", // override for your proxy's port
      "model": "gpt-5.5"                 // whatever model your proxy serves
    }
  ]
}
```

```sh
zero exec --prompt "say hi"   # routes through the proxy → your ChatGPT plan
```

### Claude via a local proxy

There is no single canonical Claude OAuth-proxy port, so use the generic
**`custom-anthropic-compatible`** entry pointed at your proxy's Anthropic-compatible
endpoint:

```jsonc
{
  "activeProvider": "claude",
  "providers": [
    {
      "name": "claude",
      "catalogID": "custom-anthropic-compatible",
      "baseURL": "http://localhost:<port>",  // your Claude proxy
      "apiKey": "unused-by-proxy",
      "model": "claude-sonnet-4.5"
    }
  ]
}
```

---

## 3. Supported alternatives (no proxy)

- **API key (recommended, simplest):** set `OPENAI_API_KEY` / `ANTHROPIC_API_KEY`
  (or per-profile `apiKey`) and use the `openai` / `anthropic` catalog providers.
  Bills as API usage.
- **Anthropic subscription automation, sanctioned:** spawn the real `claude` CLI
  (e.g. `claude -p …`) as a subprocess — the only path Anthropic recognizes as a
  first-class subscription session.

---

## Notes

- The `chatgpt-proxy` base URL / port and model are defaults you override for your
  setup; they are not an endorsement of any specific proxy implementation.
- Subscription-via-proxy depends on third-party tools and undocumented vendor
  backends; it can break without notice. The API-key path is the stable one.
