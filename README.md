# gbp

[![ci](https://github.com/jparavisini/gbp/actions/workflows/ci.yml/badge.svg)](https://github.com/jparavisini/gbp/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/jparavisini/gbp)](https://github.com/jparavisini/gbp/releases)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Minimal CLI for the Google Business Profile APIs. One binary, raw API JSON on stdout. Built for scripting: pipe to `jq`, call from CI, wrap in agent skills.

Business Profile is split across five API hosts with two naming schemes. This tool puts the parts worth scripting behind one command: accounts, locations, reviews, posts, media, performance metrics, search keywords, and verification state. No config files and no output formats to learn.

Sibling of [gsc](https://github.com/jparavisini/gsc), which does the same for Search Console.

## Install

```bash
go install github.com/jparavisini/gbp@latest
```

Or grab a prebuilt binary for macOS, Linux, or Windows from [releases](https://github.com/jparavisini/gbp/releases).

## Auth

Business Profile does not support service accounts. Google documents user OAuth only, and a service account invited as an owner still gets 404s. So `gbp` uses a refresh token for a Google account that owns or manages the profiles. You log in once, and every command after that is non-interactive.

One-time setup:

1. Request API access for a Google Cloud project with the [Business Profile API access form](https://support.google.com/business/workflow/16726127). Until Google approves the project its quota is 0 and every call returns 429.
2. In that project, enable the APIs `gbp` calls: My Business Account Management, My Business Business Information, Business Profile Performance, My Business Verifications, and Google My Business.
3. Create an OAuth client of type **Desktop app** and download its JSON. If the consent screen is in Testing status, refresh tokens expire after 7 days. Publish the app, or make it Internal if the managing account is in your Workspace org.
4. Log in. `gbp` prints a URL, you approve in the browser, and the credential is written to the path you give, mode 0600. The file is replaced only after a successful login, so a failed or abandoned login keeps the old credential. The login waits 5 minutes for the browser.

```bash
gbp login client-secret.json ~/.config/gbp/credentials.json
```

Commands resolve that credential in order:

1. `GBP_CREDENTIALS_JSON`: inline JSON (for CI secrets)
2. `GBP_CREDENTIALS`: path to the JSON file

The file holds a long-lived refresh token with manage access to every profile the account can see. Treat it like a password.

## Usage

Resources are named exactly as the API returns them. An account is `accounts/123`. A location is `accounts/123/locations/456`. `locations list` returns bare `locations/456` names, so prefix them with the account. Every command except reviews, posts, and media also accepts the bare form.

```bash
gbp accounts list

gbp locations list accounts/123
gbp locations list accounts/123 --filter 'title="Acme Plumbing"' --read-mask name,title,websiteUri
gbp locations get accounts/123/locations/456

# Overwrite only the fields named in --update-mask, from JSON on stdin
echo '{"websiteUri":"https://example.com/"}' \
  | gbp locations patch accounts/123/locations/456 --update-mask websiteUri --validate-only

gbp reviews list accounts/123/locations/456 --order-by "updateTime desc"
gbp reviews reply accounts/123/locations/456/reviews/abc "Thanks for coming in, Sam."
gbp reviews delete-reply accounts/123/locations/456/reviews/abc

gbp posts list accounts/123/locations/456
gbp posts create accounts/123/locations/456 < post.json
gbp posts delete accounts/123/locations/456/localPosts/789

gbp media list accounts/123/locations/456

# Daily metrics: impressions, calls, website clicks, direction requests (18 months of history)
gbp performance accounts/123/locations/456 --start 2026-06-01 --end 2026-06-30 \
  --metrics WEBSITE_CLICKS,CALL_CLICKS

# Search terms that surfaced the profile, by month
gbp keywords accounts/123/locations/456 --start 2026-04 --end 2026-06

# Is the profile verified and live, or suspended?
gbp verification accounts/123/locations/456
```

Output is the unmodified API response. Errors go to stderr with exit code 1. `reviews delete-reply` and `posts delete` print a confirmation to stderr and nothing to stdout. List commands take `--page-size` and `--page-token`; pass the response's `nextPageToken` back to get the next page.

```bash
# unanswered reviews, newest first
gbp reviews list accounts/123/locations/456 \
  | jq -r '.reviews[] | select(.reviewReply == null) | "\(.starRating)\t\(.name)\t\(.comment // "")"'

# total website clicks for the month
gbp performance accounts/123/locations/456 --start 2026-06-01 --end 2026-06-30 --metrics WEBSITE_CLICKS \
  | jq '[.multiDailyMetricTimeSeries[].dailyMetricTimeSeries[].timeSeries.datedValues[].value // "0" | tonumber] | add'

# every location in an account, all pages
token=""
while :; do
  page=$(gbp locations list accounts/123 --page-size 100 ${token:+--page-token "$token"})
  echo "$page" | jq -c '.locations[]'
  token=$(echo "$page" | jq -r '.nextPageToken // empty')
  [ -z "$token" ] && break
done
```

A minimal `post.json`:

```json
{
  "languageCode": "en-US",
  "topicType": "STANDARD",
  "summary": "Open until 8pm this Friday.",
  "callToAction": { "actionType": "LEARN_MORE", "url": "https://example.com/hours" }
}
```

## CI example (GitHub Actions)

```yaml
- name: Fail the build if the profile is suspended
  env:
    GBP_CREDENTIALS_JSON: ${{ secrets.GBP_CREDENTIALS_JSON }}
  run: gbp verification "$GBP_LOCATION" | jq -e '.hasVoiceOfMerchant'
```

## Not included

- **Q&A.** Google shut the Q&A API down in November 2025.
- **Media upload, admins and invitations, place action links, lodging, notifications, attributes.** The APIs exist. Nothing here has needed them yet.
- Creating, verifying, or deleting locations.
- Review removal requests and anything else that only exists in the Business Profile UI.

## License

[MIT](LICENSE)
