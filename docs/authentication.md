# Authentication

Run `lintdec provider add` to add a provider. Set `LINTDEC_ACCESSIBLE=1` for plain prompts.

## OpenRouter

**Quick setup** selects Jev and uses browser sign-in to create an API key. It sends the text being checked to OpenRouter; check your organization's data policy before sending code.

**Advanced setup** supports chat or decision models and these authentication options:

- **API key:** use an existing key from [OpenRouter settings](https://openrouter.ai/settings/keys).
- **Sign in with browser:** create an API key; no client ID is required.
- **Browser OIDC:** sign in using a registered public client ID. See the requirements below.
- **Workload identity:** supply an organization-issued rotating subject-token file and an OpenRouter federation policy ID. Your organization must configure a trusted issuer and inference-scoped policy first. Alternatively set `OPENROUTER_OIDC_TOKEN_FILE` and `OPENROUTER_FEDERATION_POLICY_ID`.

## Browser OIDC

Browser OIDC is unavailable on Windows. The issuer must support S256 PKCE and refresh tokens; its discovered authorization, token, and signing-key endpoints must share its origin. You must supply a public client ID registered with the issuer; discovery cannot supply one. OpenRouter defaults to `https://clerk.openrouter.ai`.

For another identity provider, choose **Custom OIDC** and provide its HTTPS issuer, registered client ID, API endpoint, and any needed resource and scopes. Run `lintdec provider login <name>` if sign-in is needed again. Issuers that reuse one rotating refresh token across resource grants are unsupported.

## Other providers

OpenAI and local/custom endpoints are also available. Local/custom endpoints can use chat or System One decision models, with or without an API key.

## Credential storage and removal

New API keys and browser OIDC refresh tokens require an available OS keychain. Plaintext keys in older config files are not automatically migrated. Provider configuration lives in `~/.config/lintdec/config.json`; don't put credentials in `lintdec.yaml`.

Removing an OpenRouter provider does not revoke a browser-created key; revoke it in [OpenRouter settings](https://openrouter.ai/settings/keys). Removing a provider does not remove its shared OIDC session; revoke that with the identity provider when needed.
