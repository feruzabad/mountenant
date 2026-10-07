# Configuration

| File | Purpose |
| --- | --- |
| `config.schema.json` | JSON Schema (draft 2020-12) for `config.json` |
| `users.schema.json` | JSON Schema for `users.json` |
| `config.example.json` | Starting point for `config.json` |
| `users.example.json` | Starting point for `users.json`; replace the hashes with output of `mountenant hash-password` |

Precedence (specification §8.1): built-in defaults, `config.json`
(`MOUNTENANT_CONFIG`, default `/etc/mountenant/config.json`), `users.json`
(`MOUNTENANT_USERS`, default next to `config.json`), environment variables,
then `_FILE` secrets.

Every key can be set with an environment variable named after its JSON path,
for example `server.publicUrl` → `MOUNTENANT_SERVER_PUBLIC_URL` and
`logging.securityLog.path` → `MOUNTENANT_LOGGING_SECURITY_LOG_PATH`. Arrays
take JSON; string arrays also take a comma-separated list. Append `_FILE` to
read the value from a file (Docker and Kubernetes secrets).

Secrets belong in the environment or in secret files, not in `config.json`:

```sh
MOUNTENANT_BACKEND_API_KEY_FILE=/run/secrets/backend_api_key
MOUNTENANT_BACKEND_WEBDAV_PASSWORD_FILE=/run/secrets/backend_webdav_password
# [{"id":"2026-10","secret":"<at least 32 bytes, e.g. openssl rand -base64 48>"}]
MOUNTENANT_SIGNING_KEYS_FILE=/run/secrets/signing_keys
```

Check a configuration without starting the server:

```sh
mountenant config validate
```

Unknown keys and unknown `MOUNTENANT_` variables are errors.
