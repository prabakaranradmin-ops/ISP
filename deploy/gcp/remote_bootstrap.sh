#!/usr/bin/env bash
# remote_bootstrap.sh — runs ON the GCE instance. Creates the .env and AES
# key store if they are not already there, and leaves both alone if they are.
#
# Idempotence here is a correctness requirement, not a convenience. Every
# redeploy calls this, and regenerating either file against a live database
# is destructive in a way that is not immediately visible:
#
#   - AES_KEY_STORE_URL's key decrypts the PII columns. A new key does not
#     fail loudly; it makes existing encrypted values unreadable.
#   - APP_DB_PASSWORD is what migration 019's bss_app role authenticates
#     with. Changing it in .env without changing it in PostgreSQL locks both
#     services out of the database.
#
# So: generate on absence, never overwrite.

set -euo pipefail

DOMAIN="${1:?usage: remote_bootstrap.sh <domain>}"

# 48 raw bytes then strip the base64 characters that are unsafe unescaped in
# a postgres:// DSN ('+', '/', '='), then truncate to a fixed length. The
# headroom matters: stripping removes an unpredictable number of characters,
# and config.go enforces a 32-character minimum, so starting at 64 and
# cutting to 40 is reliably long enough even in the worst case.
gen_secret() {
    head -c 48 /dev/urandom | base64 -w0 | tr -d '=+/\n' | head -c 40
}

# Replace a key's value in .env, or append it if the key is absent. Actually
# replacing rather than appending a second line matters: .env.example ships
# placeholders like "change_me_in_production_hmac_secret", and a file
# containing both the placeholder and a real value is one parser's
# disagreement away from running on the placeholder.
set_env() {
    local key="$1" value="$2"
    if grep -q "^${key}=" .env; then
        # '|' as the delimiter, and the value is base64url-safe by
        # construction (gen_secret strips '+/='), so no escaping is needed.
        sed -i "s|^${key}=.*|${key}=${value}|" .env
    else
        printf '%s=%s\n' "$key" "$value" >> .env
    fi
}

if [ ! -f .env ]; then
    echo "creating .env from .env.example"
    cp .env.example .env
    chmod 600 .env

    # Every secret gets real entropy. PORTAL_JWT_SECRET is deliberately not
    # set: docker-compose.yml does not pass it, and config.go derives it as
    # JWT_SECRET + "_portal" when empty, which already gives the portal a
    # distinct signing key from the staff console. Setting it here would be
    # config that looks load-bearing and is never read.
    set_env DOMAIN                  "$DOMAIN"
    set_env DB_SECURE_PASSWORD      "$(gen_secret)"
    set_env APP_DB_PASSWORD         "$(gen_secret)"
    set_env JWT_SECRET              "$(gen_secret)"
    set_env RADIUS_SECRET           "$(gen_secret)"
    set_env RADIUS_VERIFIER_SECRET  "$(gen_secret)"
    set_env RAZORPAY_WEBHOOK_SECRET "$(gen_secret)"
    set_env AES_KEY_STORE_URL       "local:/app/config/keys/aes_keys.json"

    echo ".env created (0600)"

    # Fail rather than start on a placeholder. A "change_me" value surviving
    # into a running deployment is a silent security hole, and it is cheap to
    # refuse here instead.
    if grep -q "change_me" .env; then
        echo "ERROR: .env still contains placeholder values after generation:" >&2
        grep -n "change_me" .env >&2
        exit 1
    fi
else
    echo ".env already exists — secrets left alone"
    # DOMAIN is the one value a redeploy may legitimately change, and it is
    # not a secret, so it is safe to update in place.
    set_env DOMAIN "$DOMAIN"
fi

mkdir -p config/keys
if [ ! -f config/keys/aes_keys.json ]; then
    echo "generating AES key store"
    KEY_B64="$(head -c 32 /dev/urandom | base64 -w0)"
    printf '{"active_version":"v1","keys":{"v1":"%s"}}\n' "$KEY_B64" > config/keys/aes_keys.json
    chmod 600 config/keys/aes_keys.json
    echo "config/keys/aes_keys.json created (0600)"

    cat <<'WARN'

  ────────────────────────────────────────────────────────────────────────
  BACK UP config/keys/aes_keys.json NOW, somewhere other than this VM.

  It is not in the PostgreSQL dump and cannot be regenerated. Losing it
  makes every encrypted PII column permanently unreadable — a database
  restore will not bring those values back.
  ────────────────────────────────────────────────────────────────────────

WARN
else
    echo "config/keys/aes_keys.json already exists — left alone"
fi
