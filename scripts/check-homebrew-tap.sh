#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# check-homebrew-tap.sh — fails when the Homebrew tap points at a release that
# no longer exists or has moved.
#
# The regression this guards: `openpanda.rb` in Xustalis/homebrew-openpanda was
# left pinned to `version "0.0.8"` after the v0.0.8 tag and release were
# deleted. Nothing complained, and every `brew install
# Xustalis/openpanda/openpanda` (the command the README and every release body
# advertise) returned 404 until someone happened to look.
#
# Read-only; works anonymously against a public repository. Exits non-zero only
# on *definitive* drift: an unreachable API (rate limit, network) reports SKIP
# instead, so a scheduled run does not go red for reasons that have nothing to
# do with the formula.
#
# Env:
#   OPENPANDA_REPO_URL   source repository (default Xustalis/OpenPanda)
#   OPENPANDA_TAP_REPO   tap repository (default Xustalis/homebrew-openpanda)
#   OPENPANDA_TAP_BRANCH tap branch (default main)
#   GITHUB_TOKEN / GH_TOKEN
#                        optional; lifts api.github.com off the anonymous
#                        60/hour budget. Only sent to api.github.com.
set -u

REPO_URL="${OPENPANDA_REPO_URL:-https://github.com/Xustalis/OpenPanda}"
REPO_SLUG="${REPO_URL#https://github.com/}"
REPO_SLUG="${REPO_SLUG%/}"
TAP_REPO="${OPENPANDA_TAP_REPO:-Xustalis/homebrew-openpanda}"
TAP_BRANCH="${OPENPANDA_TAP_BRANCH:-main}"

fail=0
note() { printf '%s\n' "$1"; }
bad()  { printf '%s\n' "$1" >&2; fail=1; }
skip() { printf 'SKIP: %s\n' "$1"; exit 0; }

command -v curl >/dev/null 2>&1 || skip "curl not available"

# http_get <url> [accept] → sets BODY and HTTP_CODE.
#
# Deliberately not a "print the body, stash the status in a variable" helper:
# every caller wants the body, so it would be called as $(http_get …), and a
# command substitution runs in a subshell — the status assigned inside it would
# never reach the caller. That is how this script used to lose the HTTP status
# entirely and print "HTTP ;" in its skip line. Callers use the two globals.
#
# An error response has a body too: api.github.com answers a rate-limited
# request with 403 and a JSON notice, and that notice is not a formula. BODY is
# therefore only set on 200, so each caller's "empty means fall back / skip"
# test actually fires.
#
# GITHUB_TOKEN (or GH_TOKEN) is used when present, and only against
# api.github.com. Anonymous api.github.com is 60 requests/hour/IP, and a hosted
# runner shares its IP with everyone else on it — which is how the weekly
# Security run kept landing on SKIP instead of checking anything. The token must
# not ride along to the release CDN: a `releases/download` URL redirects to
# objects.githubusercontent.com, which answers 401 to a bearer token it does not
# own, so an authenticated HEAD made every asset look missing.
HTTP_CODE=""
BODY=""
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
auth_header() {
    case "$1" in
        https://api.github.com/*) [ -n "$TOKEN" ] && printf 'Authorization: Bearer %s' "$TOKEN" ;;
    esac
}
http_get() {
    _url="$1"; _accept="${2:-}"
    set -- -sSL --retry 2 --max-time 30 -w '\n%{http_code}'
    [ -n "$_accept" ] && set -- "$@" -H "Accept: $_accept"
    _auth="$(auth_header "$_url")"
    [ -n "$_auth" ] && set -- "$@" -H "$_auth"
    _out="$(curl "$@" "$_url" 2>/dev/null || true)"
    HTTP_CODE="$(printf '%s' "$_out" | tail -n1)"
    case "$HTTP_CODE" in
        200) BODY="$(printf '%s' "$_out" | sed '$d')" ;;
        *)   BODY="" ;;
    esac
}

# head_status <url> → HTTP status of a HEAD request (no body).
head_status() {
    _url="$1"
    set -- -sSL -o /dev/null -w '%{http_code}' --retry 2 --max-time 30 -I
    _auth="$(auth_header "$_url")"
    [ -n "$_auth" ] && set -- "$@" -H "$_auth"
    curl "$@" "$_url" 2>/dev/null || echo 000
}

# ── 1. Read the formula ─────────────────────────────────────────────────────
# The contents API is preferred over raw.githubusercontent.com: the raw
# endpoint serves a cached copy for minutes after a push, which would report
# the previous version right when a repair is being verified.
http_get "https://api.github.com/repos/${TAP_REPO}/contents/Formula/openpanda.rb?ref=${TAP_BRANCH}" 'application/vnd.github.raw'
formula="$BODY"
if [ -z "$formula" ]; then
    # Fall back to the CDN (not rate limited) before giving up.
    http_get "https://raw.githubusercontent.com/${TAP_REPO}/${TAP_BRANCH}/Formula/openpanda.rb"
    formula="$BODY"
fi
if [ -z "$formula" ]; then
    case "$HTTP_CODE" in
        404)
            bad "check-homebrew-tap: $TAP_REPO has no Formula/openpanda.rb on $TAP_BRANCH"
            note "check-homebrew-tap: FAILED"
            exit 1
            ;;
        *) skip "could not read the tap formula (HTTP $HTTP_CODE; rate limit or network?)" ;;
    esac
fi

version="$(printf '%s\n' "$formula" | sed -n 's/^ *version "\([^"]*\)".*/\1/p' | head -n1)"
if [ -z "$version" ]; then
    bad "check-homebrew-tap: no 'version \"...\"' line in the formula"
    note "check-homebrew-tap: FAILED"
    exit 1
fi

# ── 2. Does that release still exist, and is it stable? ─────────────────────
http_get "https://api.github.com/repos/${REPO_SLUG}/releases/tags/v${version}" 'application/vnd.github+json'
meta="$BODY"
if [ -z "$meta" ]; then
    if [ "$HTTP_CODE" = 404 ]; then
        bad "check-homebrew-tap: formula pins v$version but that release does not exist (brew install is a 404)"
        note "check-homebrew-tap: FAILED"
        exit 1
    fi
    skip "could not read release metadata for v$version (HTTP $HTTP_CODE; rate limit or network?)"
fi

case "$meta" in
    *'"draft": true'*) bad "check-homebrew-tap: v$version is still a draft release";;
esac
case "$meta" in
    *'"prerelease": true'*)
        bad "check-homebrew-tap: formula pins pre-release v$version; the tap must track a stable tag" ;;
esac

# ── 3. Are the four archives the formula references actually downloadable? ──
for target in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
    url="$REPO_URL/releases/download/v${version}/panda-${version}-${target}.tar.gz"
    code="$(head_status "$url")"
    case "$code" in
        200|302) ;;
        *) bad "check-homebrew-tap: asset for $target returned HTTP $code ($url)";;
    esac
done

# ── 4. Do the recorded sha256 values still match the published checksums? ───
http_get "$REPO_URL/releases/download/v${version}/checksums.txt"
sums="$BODY"
if [ -z "$sums" ]; then
    bad "check-homebrew-tap: v$version publishes no checksums.txt"
else
    for target in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
        archive="panda-${version}-${target}.tar.gz"
        want="$(printf '%s\n' "$sums" | awk -v f="$archive" '$2==f { print $1; exit }')"
        [ -n "$want" ] || { bad "check-homebrew-tap: checksums.txt has no $archive"; continue; }
        # Requiring the published value to appear somewhere in the formula is
        # enough to catch one rendered against a different build.
        printf '%s\n' "$formula" | grep -q "$want" \
            || bad "check-homebrew-tap: formula sha256 for $archive does not match the published checksum"
    done
fi

if [ "$fail" -ne 0 ]; then
    note "check-homebrew-tap: FAILED (repair with scripts/sync-homebrew-tap.sh $version)"
    exit 1
fi
note "check-homebrew-tap: OK (tap pins v$version; release, assets and checksums verified)"
