# Usage

## Requirements

- CLIProxyAPI v8 with dynamic plugin support and the quota provider.
- A CPA build that accepts the plugin's declared ABI and RPC schema: the plugin
  registers ABI 1 and schema 10 from its pinned SDK, and an older host refuses it at
  registration with `plugin schema version 10 is not supported`.
- **CPA v8.0.49 or later** to load the plugin (schema 10 first shipped there), and
  **CPA v8.0.51 or later** for the separate WorkBuddy and WorkBuddy AI sign-in cards:
  per-entry name and logo first shipped in v8.0.51. On an older host both entries
  still work; a panel that predates the field shows one card per *identifier*
  decorated with the plugin's own name and logo.
- Go 1.27.1 for local development. Release builds are produced by CI.

Sign-in is not covered by an offline test. The unit tests drive the OAuth calls
through a fake sender, so the requests and responses the plugin builds are checked,
but a live browser sign-in and a live chat need real WorkBuddy and WorkBuddy AI
accounts. Verify a login against your own accounts before relying on a build, and
report what you observe.

## Install from the plugin store

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-workbuddy-oauth:
      enabled: true
      max_concurrency: 3
      app_version: 5.9.3
      cli_version: 2.165.0
      # workbuddy (default) or workbuddy-ai. Only a fallback: a login request that
      # names an entry picks that entry's build.
      site: workbuddy
      # Model ids this plugin never advertises. WorkBuddy's product config lists
      # models an account's plan does not include, and that is only learned by
      # being refused. Naming them here keeps CPA from routing those models to
      # this credential at all, which matters when the credential has the highest
      # priority and no other provider serves the model.
      exclude_models: []
      # exclude_models: [gpt-6-luna, gemini-3.8-flash]
```

Use CPA's plugin store to install `omc-workbuddy-oauth`, or call:

```text
POST /v0/management/plugin-store/omc-workbuddy-oauth/install
POST /v8/management/plugins/store/omc-workbuddy-oauth/install
```

Add `?version=<version>` to install a specific release. The store requires the
release to carry `omc-workbuddy-oauth_<version>_<os>_<arch>.zip` and
`checksums.txt`, and verifies the archive against the checksum before unpacking it.

The archive contains one library file and is installed under
`<plugins-dir>/<os>/<arch>/` as `omc-workbuddy-oauth-v<version>.so`, `.dylib` or
`.dll`, depending on the target platform: the installed version is part of the file
name. CPA also loads a hand-placed unversioned `omc-workbuddy-oauth.so`; that file
carries no version, so a pinned version in `plugins.configs` does not match it.
Restart CPA if it reports that the loaded plugin must be replaced.

Manual layout, for the version you install:

```text
<plugins-dir>/linux/amd64/omc-workbuddy-oauth-v<version>.so
<plugins-dir>/linux/arm64/omc-workbuddy-oauth-v<version>.so
<plugins-dir>/darwin/amd64/omc-workbuddy-oauth-v<version>.dylib
<plugins-dir>/darwin/arm64/omc-workbuddy-oauth-v<version>.dylib
<plugins-dir>/windows/amd64/omc-workbuddy-oauth-v<version>.dll
<plugins-dir>/windows/arm64/omc-workbuddy-oauth-v<version>.dll
```

## Description declaration

A release may publish an optional `<plugin_id>.omc.json` (`schema_version` 1) with
localized descriptions. When it does, the file is `omc-workbuddy-oauth.omc.json` and
the plugin serves the same bytes at:

```text
GET /v0/resource/plugins/omc-workbuddy-oauth/omc.json
```

The parts that matter here:

- `plugin_id` is `omc-workbuddy-oauth`, and `plugin_version` is the release version
  without a leading `v`. A document naming another id or version is ignored, not
  guessed at.
- `i18n.description` may contain any subset of `zh`, `en`, `zh-Hant` and `ms`; each
  value is plain text.
- It declares no menu, settings label or action, and it never changes the plugin
  name, logo or the description CPA's store already shows.
- CPA serves plugin resources without management authentication, and answers 404 for
  them in Home mode. When the resource is unavailable, a reader uses the release
  asset; when neither exists, the description in the official plugin source is the
  result.

The served body is never re-serialized: the plugin embeds the same file that
packaging copies into the release, so served and published bytes are comparable by
hash. Bump the file together with the plugin version.

## Build locally

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-workbuddy-oauth.so .
```

Name the output with your platform's suffix: `.so` on Linux, `.dylib` on macOS,
`.dll` on Windows. `build/` is ignored by git. Official release artifacts for Linux,
macOS and Windows on both amd64 and arm64 are built by the release workflow when a
`v*` tag is pushed.

## Sign in

The plugin registers **two OAuth login entries**, one per build:

| Entry identifier | Build | API host |
| --- | --- | --- |
| `workbuddy` | WorkBuddy (China) | `https://copilot.tencent.com` |
| `workbuddy-ai` | WorkBuddy AI (international) | `https://www.workbuddy.ai` |

The China build's card carries WorkBuddy's own mark; the international build's
carries CodeBuddy's, because that build's assets are served from CodeBuddy's hosts.

Both produce the same credential shape (`"type": "workbuddy"`) and use the same
executor. On a panel that supports it, each entry renders as its own card with its
own name and mark (CPA v8.0.51+); the identifier is what CPA routes the login, poll
and callback by.

Start a login through CPA's shared OAuth endpoint, which is the one plugin providers
use:

```text
GET /v8/management/oauth/auth-url?provider=workbuddy
GET /v8/management/oauth/auth-url?provider=workbuddy-ai
```

The caller must supply the management key in the `Authorization: Bearer ...`
header. Do not put the key in a URL. Use the URL returned by CPA to complete the
sign-in in the browser. A request without that header cannot initiate a flow.
Public plugin resources serve only the static `logo.png`, `workbuddy.svg`,
`codebuddy-color.svg` and `omc.json` assets; other paths answer 404 and a non-GET
request answers 405.

Build resolution order is: the entry the request names, then a `site` value in the
login request's metadata, then the configured `site`, then WorkBuddy. The entry
wins, so `provider=workbuddy&site=workbuddy-ai` signs in to the China build. `site`
matters only for a login start that names no entry, for example a host that calls
the plugin under its own identifier; an explicit invalid value there is rejected
rather than falling back to the configuration.

### Flow

1. The plugin asks the build for a sign-in page:
   `POST /v2/plugin/auth/state?platform=workbuddy` (or `workbuddy-ai`). WorkBuddy
   answers the flow's `state` and the page to open.
2. The page is Tencent's own, the one WorkBuddy's desktop opens. The plugin adds its
   claimed desktop version (`version`) and a fresh `loginSessionId`, which the page
   reads to decide what it offers for that build, and returns the URL. The page
   finishes in the browser tab.
3. The plugin polls `GET /v2/plugin/auth/token?state=<state>` with WorkBuddy's user
   agent. A token completes this step; code `11217` means the page is still open and
   keeps the flow pending; a transient status (`408`, `429`, `5xx`) keeps it pending
   too; another `4xx` ends the flow with WorkBuddy's message.

   The page finishes at WorkBuddy's `/login/started`, which asks the browser to open
   the WorkBuddy desktop app. **Cancel that prompt.** It is the vendor's hand-off to
   its own client, the sign-in is already complete at that point, and CPA has the
   token: the panel shows the credential either way.
4. `GET /v2/plugin/login/account?state=<state>` names the account: its uid, nickname,
   phone number, and an enterprise id and name when it is a team or education
   account. Code `12151` means the account is not named yet and keeps the flow
   pending. A failure here is not fatal when the token answer names a uid.
5. The credential is saved with the build as `site` and `oauth_provider`.

The opaque state CPA carries is sealed with AES-GCM under
`<auth-dir>/.workbuddy-oauth-state.key` and holds the build, the flow nonce and the
expiry. WorkBuddy's own `state` is kept beside the flow in the plugin process, under
the nonce, because it is long enough that sealing it would not fit CPA's
128-character state limit. Keep the auth directory private and writable.

A plugin restart between start and poll therefore ends an unfinished sign-in; the
poll answers `sign-in state is gone; sign in again` instead of asking WorkBuddy about
a state it no longer knows. Flows expire after five minutes, the window WorkBuddy's
own client allows, and at most 64 are kept at once.

Poll a pending flow with the state CPA returned:

```text
GET /v8/management/oauth/status?state=<opaque-state>
```

`/v0/management/get-auth-status?state=<opaque-state>` is the v0 alias. Cancel with
`DELETE /v8/management/oauth/session?state=<opaque-state>` (or
`DELETE /v0/management/oauth-session?state=<opaque-state>`). Cancellation is handled
by CPA; the plugin does not implement a separate cancellation endpoint.

### Credential

The saved file is `workbuddy-<hash>.json` and carries `"type": "workbuddy"`, the
build as `site`, the same value as `oauth_provider`, the account identity, the
enterprise when there is one, and the tokens:

| Field | Meaning |
| --- | --- |
| `access` | the access token model calls and billing reads carry |
| `refresh` | the refresh token, which WorkBuddy rotates on every use |
| `expires` | when the access token ends, in epoch milliseconds |
| `refresh_expires` | when the refresh token ends, when WorkBuddy says |
| `domain` | the `X-Domain` the token was issued for |
| `uid`, `name`, `phone`, `email` | the account it belongs to |
| `enterprise_id`, `enterprise_name` | the team the sign-in was bound to, if any |

A card label is the person's name, else a masked phone number, else the email, else
the uid; an enterprise account appends ` · <enterprise name>`, because one person's
personal and enterprise accounts share the uid and the nickname.

Refresh spends `POST /v2/plugin/auth/token/refresh` with the refresh token in
`X-Refresh-Token`, `X-Auth-Refresh-Source: plugin`, the account's `X-Domain` and its
enterprise headers. Concurrent refreshes for one credential share one exchange, so a
rotating token is never spent twice. The plugin asks CPA to renew about five minutes
before the access token ends. A refresh refused with `401` or `403` means the
credential needs a new sign-in. An existing `"type": "workbuddy"` auth file is
recognized and republished when CPA imports it.

A desktop sign-in is not offered. WorkBuddy's desktop keeps its sign-in in a
per-user file on the machine the app runs on, and recent builds seal it so only the
app opens it; a plugin inside a server-side CPA cannot read it by design.

## Models and routing

Model IDs are the ids of the build's `/v3/config`, lowercased for CPA routing. The
plugin reads the `cli` agent's model list from that config once per credential and
caches it for ten minutes; a lookup that misses refreshes it once.

- Context limits, output limits, image support and reasoning levels come from the
  config. A model whose config names no levels but does reason is advertised with
  `low`, `high` and `max`; a model that cannot turn thinking off is advertised
  without `none`.
- The international build's GLM models are advertised as text-only: WorkBuddy AI
  removes images from a chat to them and answers with a system reminder saying so,
  whatever its config claims. The China build's GLM models do read images.
- The China build serves its Claude models on Anthropic's Messages API, not chat.
  This executor speaks chat completions, so those models are not advertised: a chat
  to one is refused by the build. Listing a model that cannot be served was the
  worse answer.
- A failed read keeps the last good list for that account; with no cached list at
  all, the shipped fallback is used. The China build ships fifteen entries (Auto,
  Hy4 preview, Hy3, Hy3-X, DeepSeek, GLM, MiniMax and Kimi tiers); the international
  build ships sixteen (its Default/Fast/Balanced/Primary/Deep tiers plus GPT,
  Gemini, GLM, Hy3, Kimi and MiniMax).
- A `vendor/model` prefix is trimmed before lookup, so a prefixed ID resolves to the
  same entry.
- A model WorkBuddy refuses as not included in the account's plan — the
  international build answers `model [gpt-6.1-sol] is not allowed for user type
  [personal_free_user]` for a model its own config lists — is dropped from that
  credential's list for twelve hours and reported to the client as a `400` with code
  `unknown_model`, worded as the model not being available for the plan. Hiding it
  keeps a probe, a route or a client from reaching the same refusal again, and the
  code is what keeps CPA's router treating the refusal as a credential-model
  mismatch — the code, not the wording, so the request falls through to another
  credential or provider instead of ending. The refusal is remembered across
  restarts, and the window lapses on its own, so a plan that gains the model gets
  it back without a restart.

Execution is OpenAI chat completions in and out: the plugin receives OpenAI chat
completion requests and answers in OpenAI chunks or in one OpenAI completion. CPA
translates Claude Messages, Responses and Gemini clients to OpenAI for this executor.

## Execution details

Every chat is sent to `<api host>/v2/chat/completions` with the headers WorkBuddy's
own desktop sends:

- `Authorization`, `X-User-Id`, `X-Domain`, `X-Product: SaaS`, `X-IDE-Type: WorkBuddy`,
  and `X-Enterprise-Id`/`X-Tenant-Id` for an enterprise account;
- the desktop CLI's `User-Agent`, `WorkBuddy/<app> <appName>/<app> CLI/<cli>`, because
  a request that does not start `WorkBuddy/<version>` is refused with error `10085`;
- the client attribution its usage list reads: `X-Agent-Purpose: conversation`,
  `X-IDE-Name: WorkBuddy`, `X-IDE-Version`, `X-Agent-Intent: craft`,
  `X-Requested-With`;
- the model request's own: `X-Agent-Type: main`, `X-Agent-Runtime: local-agent`, and
  the `X-Stainless-*` headers of the OpenAI SDK the CLI bundles;
- the conversation's ids: `X-Conversation-ID`, `X-Conversation-Request-ID`,
  `X-Conversation-Message-ID` and `X-Request-ID`.

The ids are derived from the chat, never from a random value per request: the
conversation's from the client's session id (CPA's `x-session-id`) or the first user
message, the turn's from the last user message, and they are scoped to the account,
so one conversation does not carry one account's ids under another's user id. A
header the caller already set is kept. A tool loop therefore stays one request in
the account's usage detail, and the user's next message becomes the next request.

WorkBuddy streams every chat, and the plugin asks WorkBuddy for the usage
chunk on every request so both streaming accounting/translators and non-streaming
aggregation receive it. A non-streaming client request is answered from that
stream: content, reasoning, tool calls and usage are assembled into one
`chat.completion`.

The request body is shaped for the endpoint:

- `max_completion_tokens` becomes `max_tokens`, and `developer` becomes `system`.
- A chat that does not open with a system message is given WorkBuddy's own default
  one, as its app does.
- An assistant turn's `reasoning_content` or `reasoning_details` becomes
  `reasoning`, the field WorkBuddy reads; WorkBuddy drops the other two.
- A chat that names no reasoning level is given the model's default one, unless its
  output budget is 1024 tokens or less: such a request cannot have meant to think,
  and WorkBuddy would spend the whole budget on reasoning.
- Identity lines WorkBuddy refuses as an "unapproved channel" (error `11128`) are
  reworded, including Claude Code's billing header and the DeepSeek Harness opener.
  The list is what has been met so far, not a rule; a refusal that survives it is
  reported as a 403 with WorkBuddy's own message.

Token counting is not supported and answers `501`. WorkBuddy documents no
token-count endpoint, and a guess would misbill clients that trust it.

## Quota

```text
GET  /v8/management/plugins/omc-workbuddy-oauth/quota
POST /v8/management/plugins/omc-workbuddy-oauth/quota
{"auth_index":"<auth index>"}
```

`GET` describes the provider: `workbuddy`, display name `WorkBuddy`, reset not
supported. `POST` fetches one credential's credits from
`POST /billing/meter/get-user-resource-summary` and the plan's cycle from
`POST /billing/meter/get-user-resource` beside it. Neither call renews the
credential: WorkBuddy rotates the refresh token on every use and a quota read has
nowhere to save the new one, so the host renews on its own schedule.

The response is normalized into a subscription and one group of buckets:

- `Paid plan`, `Free plan`, or `Enterprise` for a team account. WorkBuddy's summary
  says whether the account is paid, not which plan it bought, so a paid account is
  named as one rather than given a name the vendor never said.
- `Credits` is the plan's own credits; `Extra credits` is every other kind
  (benefits, top-ups, activity). When both exist, `Total credits` is the account's
  whole, and it carries the plan's reset time. A summary that names no credit groups
  is split by package: the subscription's package, or the free plan's daily slice,
  against everything else.
- `Prepaid credits` appears when a package has credits left but no total: it is a
  balance, so its description carries the amount.
- Each bucket reports the remaining fraction and a `N / M credits left` description.
  The plan's window and the account's total also carry a reset: the cycle end of the
  package the summary names as the subscription, or — for an account whose summary
  names none, which is every free account — of its `CapacityType 4` slice. The
  summary states amounts only, so the cycle is read from the package meter, and both
  clocks are UTC+8. A cycle already over, a package the list does not carry, or a
  package meter that cannot be read leaves the reset out rather than guessing one,
  because the amounts are still a true reading.
- A team account is read from `/billing/meter/get-enterprise-user-usage`, the meter
  WorkBuddy's own usage page uses, because its resource summary answers with no
  packages.

An account with no credits at all reports `0 credits left` rather than no data.
Resets happen on WorkBuddy's own usage page; a reset request answers with that
message instead of performing one.

## Error handling

Upstream refusals are classified by WorkBuddy's own codes and status, and the client
sees the mapped status:

| Upstream | Client sees |
| --- | --- |
| `401` | `401`, sign-in expired, sign in again |
| `402` | `402`, credits are spent |
| `403`, code `11128` (unapproved channel), `11153`, `11158` (no entitlement) | `403` with WorkBuddy's message |
| `429`, code `14018` | `429`, rate limited, with WorkBuddy's message including when the allowance resets |
| `400`, code `10085` (client identity refused) | `400` with WorkBuddy's message |
| A `400` saying `model [...] is not allowed for user type [...]` | `400`, code `unknown_model`, worded as the model not being available for the plan; the model is hidden from that credential's list for twelve hours |
| A `400` saying `prompt is too long` | `400`, worded as the prompt being longer than the model takes |
| Any other `4xx` | The upstream status with WorkBuddy's message and code |

The `14018` refusal is model-scoped on purpose: WorkBuddy's own message says
another model still works, so only the reporting model rests. CPA v8.0.62 and
later keep that scope; the plugin never asks CPA to cool the whole credential.
| `5xx` | `502` |
| Transport failure, or an unavailable upstream stream | `503` |
| The caller closed the request before WorkBuddy answered (`context canceled`) | `499`, code `client_closed` |
| WorkBuddy did not answer within the host's limit (`deadline exceeded`) | `504`, code `upstream_timeout` |

Other behaviour worth knowing:

- A request that never got an answer is reported for what it was: `499` when the
  caller went away, `504` when the host's time limit ran out, `503` otherwise. CPA
  reads `499` as the client's own cancellation and does not rest the credential for
  it, so a caller's short timeout never turns into a credential warning.
- A streaming request is answered as soon as WorkBuddy's stream opens: the plugin
  spends at most two seconds looking for a refusal in the stream's first event, so a
  reasoning model's thinking is never spent before the client's headers go out.
- The plugin does not retry a refusal in place. It answers with the mapped status so
  CPA can decide whether another credential should carry the request.
- A refusal that arrives inside an `OK` stream keeps its real status: streaming
  requests read up to the first answer event before any chunk reaches the client.
- A stream event is forwarded as a complete `data: <json>\n\n` SSE event without
  `[DONE]`. The host's deframer normalizes same-format streams while cross-format
  translators (such as OpenAI to Anthropic) parse the `data:` prefix directly, and
  the host writes the terminal marker itself.
- A non-streaming answer with no text, no reasoning and no tool call is reported as
  `502` rather than an empty `200`.
- Panics are caught at the plugin boundary and answered as a machine-coded error, and
  client-facing messages never carry token bytes or internal paths.

## Troubleshooting

- **The provider is absent:** enable the global `plugins` switch and
  `plugins.configs.omc-workbuddy-oauth.enabled`, then restart CPA if the library was
  replaced.
- **`unknown login entry: use workbuddy or workbuddy-ai` or `invalid login site: use
  workbuddy or workbuddy-ai`:** the login was started with another provider key or
  with an invalid `site` value. Start it from the `workbuddy` or `workbuddy-ai`
  entry, or set the native `site` field to one of those two values.
- **Login stays pending:** complete the browser page for the build you started. The
  flow expires after five minutes; start a new sign-in after that. If the plugin was
  restarted mid-flow, the poll answers that the state is gone.
- **`sign-in has expired; sign in again`:** the refresh token was refused, so the
  stored credential cannot be renewed. Start a new sign-in.
- **No models appear:** the config read failed and the shipped fallback took over, or
  the credential is not active. Check CPA logs for `model_error` and the credential
  status.
- **`unknown WorkBuddy model`:** the requested key is not in the account's list, or it
  is one of the China build's Claude models, which this executor does not serve. List
  the models from the panel and use the lowercase ID it shows.
- **`WorkBuddy refused this client's identity (10085)`:** the claimed desktop version
  is too old for what the endpoint expects. Set `app_version` and `cli_version` to
  the build WorkBuddy's update feed currently ships.
- **`WorkBuddy refused an agent identity line in this prompt (11128)`:** the client's
  system prompt carries a line the plugin does not know how to reword yet. Report it
  with the offending line, and use a client whose opening the plugin already knows in
  the meantime.
- **`499` with `context canceled`:** the caller stopped waiting, not WorkBuddy. The
  plugin reports it as `499` / `client_closed`, and CPA does not count it against the
  credential. Up to 0.1.2 a **streaming** request could earn this on its own: the
  plugin read until the stream's first answer event before it answered the client, and
  a reasoning model that thinks for a minute sent nothing at all in that time, so the
  caller — or a proxy in between — gave up on headers that never came.
  `deepseek-v4.1-flash-sg` is reasoning-only with `defaultEffort` `high`, which makes
  that window the worst it gets. From 0.1.3 the stream is handed to the client as soon
  as it is open, so an upgrade is the first thing to try. If it still happens, the
  caller's own timeout is the cause: read the stream with a longer one.
- **`504` with `upstream_timeout`:** the host's own limit ran out before WorkBuddy
  answered; raise CPA's timeout for the route, or stream it.
- **`403` on a model that is listed:** the account is not entitled to that model
  (WorkBuddy code `11158`), or its plan does not include it.
- **The credential shows a warning like `WorkBuddy AI: gpt-6.1-sol is not available
  for your plan (personal_free_user)`:** WorkBuddy listed a model its plan does not
  include for this account, and a request for it was refused. From 0.1.1 the plugin
  reports that as a `400` with code `unknown_model` and hides the model from the
  credential for twelve hours, so it is neither offered nor retried. The code is what
  makes CPA's router treat the refusal as a credential-model mismatch and fall
  through to another credential or provider rather than ending the request, so it
  does not depend on the message's wording. If no other
  provider in the deployment serves that model, the request still fails with this
  message: there is nothing to fall through to, which is the routing layer's
  answer, not the plugin's. To keep such a model off this credential from the first
  request, name it in `exclude_models`. The warning CPA
  already recorded is cleared by the next successful request on that credential.
  Nothing is wrong with the sign-in, and signing in again changes nothing.
- **Login fails with `CPA auth directory unavailable`:** CPA reported no auth
  directory to the plugin. Set one, and keep it writable: it holds the state key and
  the pending flows.
