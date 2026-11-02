#!/usr/bin/env bash
# One-time Pulumi setup for sevi: logs in (local state), creates the `prod` stack and
# sets every config value. Re-run it any time; it only changes what you answer.
#
# Values come from, in order: environment variables, files already in this repo
# (secret.json, neon.json, gcloud), then a prompt. Secrets are typed hidden and
# never echoed. Environment overrides:
#   SEVI_PROJECT_ID  BILLING_ACCOUNT  CLOUDFLARE_API_TOKEN  CLOUDFLARE_ACCOUNT_ID
#   OPENROUTER_API_KEY  TELEGRAM_BOT_TOKEN  DATABASE_URL  PULUMI_PASSPHRASE  CF_SKIP_VERIFY=1
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.pulumi/bin:$PATH"
STACK=prod

say()  { printf '\n==> %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is not installed. $2"; }

need pulumi "Install it: curl -fsSL https://get.pulumi.com | sh"
need gcloud "Install the Google Cloud CLI."
need docker "Docker builds and pushes the image."
need go "Go builds the Pulumi program and the app."
need python3 "Used to read the JSON files."

# ask NAME "prompt" [secret]  -> value from env var NAME, else prompt
ask() {
  local name=$1 prompt=$2 secret=${3:-} value=${!1:-}
  if [ -z "$value" ]; then
    [ -t 0 ] || die "$name is not set and there is no terminal to ask on"
    if [ -n "$secret" ]; then read -r -s -p "$prompt: " value; echo; else read -r -p "$prompt: " value; fi
  fi
  value=$(clean "$value")
  [ -n "$value" ] || die "$name is required"
  printf '%s' "$value"
}
# clean keeps only visible ASCII: it drops spaces, newlines, paste markers and invisible unicode (zero-width spaces).
clean() { printf '%s' "$1" | sed -e 's/\x1b\[20[01]~//g' | LC_ALL=C tr -cd '\041-\176'; }
json_get() { python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get(sys.argv[2],""))
except Exception: print("")' "$1" "$2"; }

say "Checking Google login"
adc=${GOOGLE_APPLICATION_CREDENTIALS:-$HOME/.config/gcloud/application_default_credentials.json}
[ -s "$adc" ] || die "run: gcloud auth application-default login  (a plain 'gcloud auth login' is not enough)"

say "Pulumi passphrase (encrypts the secret config; kept in deploy/.passphrase, mode 600)"
if [ ! -s .passphrase ]; then
  pp=$(ask PULUMI_PASSPHRASE "Choose a passphrase" secret)
  (umask 077; printf '%s' "$pp" > .passphrase)
fi
export PULUMI_CONFIG_PASSPHRASE_FILE="$PWD/.passphrase"

say "Pulumi login (state stays on this machine, in ~/.pulumi)"
pulumi login --local >/dev/null
pulumi stack select "$STACK" >/dev/null 2>&1 || pulumi stack init "$STACK" >/dev/null
cfg()  { pulumi config set --stack "$STACK" "$1" "$2" >/dev/null; }
scfg() { printf '%s' "$2" | pulumi config set --stack "$STACK" --secret "$1" >/dev/null; }

say "GCP project and billing"
project=${SEVI_PROJECT_ID:-$(pulumi config get sevi:projectId --stack "$STACK" 2>/dev/null || true)}
if [ -z "$project" ]; then
  default="sevi-$(whoami | tr -cd 'a-z0-9')-$RANDOM"
  [ -t 0 ] && read -r -p "New GCP project id (globally unique) [$default]: " project || true
  project=${project:-$default}
fi
[[ $project =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "project id must be 6-30 chars: lowercase letters, digits, hyphens"
cfg sevi:projectId "$project"
# Make Google API calls use the new project for quota (avoids "requires a quota project").
cfg gcp:userProjectOverride true
cfg gcp:billingProject "$project"
cfg gcp:disableGlobalProjectWarning true
billing=${BILLING_ACCOUNT:-$(gcloud billing accounts list --filter=open=true --format='value(name)' 2>/dev/null | sed 's#.*/##')}
[ "$(printf '%s\n' "$billing" | grep -c .)" = 1 ] || billing=$(ask BILLING_ACCOUNT "Billing account id (gcloud billing accounts list)")
cfg sevi:billingAccount "$billing"
echo "project $project, billing $billing"

say "Cloudflare (R2 audio bucket)"
cf_help() {
  cat <<'TXT'
You need an API token. Create one at https://dash.cloudflare.com/profile/api-tokens
  -> Create Token -> Create Custom Token, with these permissions:
       Account | Workers R2 Storage | Edit
       Account | Account Settings   | Read
       User    | API Tokens         | Edit
  Account Resources: Include -> your account. Create it and copy the token.
TXT
}
# Ask for the token and account id, check them with Cloudflare, and store them.
cf_setup() {
  cf_help
  cf_token=$(ask CLOUDFLARE_API_TOKEN "Cloudflare API token" secret)
  cf_account=$(clean "${CLOUDFLARE_ACCOUNT_ID:-}")
  # The token goes in on stdin, so it never shows in the process list.
  cf_api() { printf 'Authorization: Bearer %s' "$cf_token" | curl -sS -H @- "https://api.cloudflare.com/client/v4$1"; }
  if [ "${CF_SKIP_VERIFY:-}" != 1 ]; then
    resp=$(cf_api /user/tokens/verify) || die "could not reach Cloudflare"
    printf '%s' "$resp" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("success") else 1)' 2>/dev/null \
      || die "Cloudflare did not accept that token: $(printf '%s' "$resp" | head -c 300)"
    if [ -z "$cf_account" ]; then
      cf_account=$(cf_api /accounts | python3 -c 'import json,sys; r=json.load(sys.stdin).get("result",[]); print(r[0]["id"] if len(r)==1 else "")' 2>/dev/null || true)
    fi
  fi
  [ -n "$cf_account" ] || cf_account=$(ask CLOUDFLARE_ACCOUNT_ID "Cloudflare account id (dashboard home, right side)")
  cfg sevi:cloudflareAccountId "$cf_account"
  scfg cloudflare:apiToken "$cf_token"
  echo "cloudflare account $cf_account"
}
have_cf=$(pulumi config get sevi:cloudflareAccountId --stack "$STACK" 2>/dev/null || true)
pulumi config get cloudflare:apiToken --stack "$STACK" >/dev/null 2>&1 || have_cf=""
if [ -n "$have_cf" ] && [ -z "${CF_RESET:-}" ] && [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
  echo "already set (account $have_cf); run with CF_RESET=1 to change it"
else
  cf_setup
fi

say "Keys and database"
OPENROUTER_API_KEY=${OPENROUTER_API_KEY:-$(json_get ../secret.json OPENROUTER_API_KEY)}
TELEGRAM_BOT_TOKEN=${TELEGRAM_BOT_TOKEN:-$(json_get ../secret.json TELEGRAM_BOT_TOKEN)}
DATABASE_URL=${DATABASE_URL:-$(json_get neon.json DATABASE_URL)}
scfg sevi:openrouterApiKey "$(ask OPENROUTER_API_KEY "OpenRouter API key" secret)"
scfg sevi:telegramBotToken "$(ask TELEGRAM_BOT_TOKEN "Telegram bot token" secret)"
scfg sevi:databaseUrl "$(ask DATABASE_URL "Neon DATABASE_URL" secret)"
jina=$(json_get ../secret.json JINA_API_KEY); [ -z "$jina" ] || scfg sevi:jinaApiKey "$jina"

[ -f ../config.prod.json ] || die "../config.prod.json is missing (the image bakes it in)"

say "Done. Config for stack '$STACK':"
pulumi config --stack "$STACK"
cat <<'TXT'

Next, from the repo root (close the browser and editor first: the build needs ~4 GB):
  make infra_preview
  make infra_up
TXT
