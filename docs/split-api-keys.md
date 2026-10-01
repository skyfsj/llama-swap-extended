# Split API keys: management keys and access keys

The API key surface is split into two kinds so a credential that talks to
models can never administer the deployment.

- **Management keys** (`kind: management`) sign in to the control panel and
  administer everything: models, runtimes, settings, other keys. They are the
  only credentials accepted by `POST /api/auth/session`, so the browser login
  only accepts them. Keys that existed before the split are management keys,
  which keeps every previously issued credential working.
- **Access keys** (`kind: access`) are model-traffic credentials nested under a
  management key. They carry the restrictions and always authenticate with the
  `inference` scope alone: an access key can never read the logs, the audit
  table, or the configuration, and it can never sign in to the panel — even if
  its management parent has full scopes.

A single permanent management key must always remain (the control plane refuses
to revoke the last one), so the deployment cannot lock itself out.

## Access key restrictions

| Field | Meaning |
| --- | --- |
| `allowedIps` | Caller addresses or CIDR blocks the key may be used from. Empty means unrestricted. The check uses the same resolved address the access log prints (`X-Forwarded-For`, then `X-Real-IP`, then the connection address), so a rejected request names an address you can match against the log. |
| `models` | Model allowlist, intersected with the parent's. A model the parent cannot reach stays unreachable, and a restriction with no overlap is rejected at write time instead of silently widening to "all models". Exact names and a single leading or trailing `*` are supported. |
| `maxConcurrency` | Ceiling on simultaneous in-flight requests per key. Requests beyond the ceiling get `429 Too Many Requests`. A slot is held for the whole response, so a streaming request counts for its entire lifetime. `0` is unlimited. |
| `group` | Free-form label used to group rows on the usage records page. |
| `expiresAt` | Termination time. An access key can never outlive its management key: creating or updating one beyond the parent's expiry is rejected. |

Revoking a management key revokes every access key nested under it in the same
transaction. Narrowing a management key's `models` also narrows its active
access keys, so a model removed from the parent stops being reachable through a
child.

An access key whose parent is missing, revoked, or expired fails closed and
authenticates as invalid.

## Managing keys

The **API keys** page in the Web UI (`/keys`) lists management keys with their
nested access keys. Each management key row expands to its access keys, where
the IP allowlist, concurrency ceiling, usage group, models and expiry are
edited. The secret is shown exactly once at creation or rotation.

The HTTP surface lives under `/api/keys`:

```
GET    /api/keys?include_revoked=true      # both kinds, with live in-flight counts
POST   /api/keys                           # kind, parentId, models, allowedIps, maxConcurrency, group, expiresAt
PATCH  /api/keys/{id}                      # same fields; kind and parentId are immutable
DELETE /api/keys/{id}                      # revokes nested access keys too
POST   /api/keys/{id}/rotate               # new secret, same restrictions
```

## Usage records

The **usage records** page (`/usage`, "Usage records" in the observation
group) turns the activity log into a dashboard:

- summary cards for total requests, total tokens (with input/output/cache
  breakdown), estimated cost and average duration
- distribution donuts with tables for models, usage groups and endpoints
- a token trend chart (input, output, cache creation, cache read) with a cache
  hit-rate line
- filters by time range, granularity, API key, model, group and endpoint
- a detail table (key, model, endpoint, caller IP, group, tokens, cost,
  latency, time) paginated and sortable server-side
- CSV export of the full filtered selection

Every number is derived from the activity log, so it is scoped to what the
activity log retained, and the cost is an estimate from the pricing catalog
rather than billing data. Model-scoped and key-scoped credentials see only their
own traffic, exactly like the activity and audit endpoints.

The page reads four endpoints, so changing a filter only re-fetches the part of
the dashboard that depends on it:

```
GET /api/usage/analytics?granularity=day&start=...&key=...&model=...&group=...&endpoint=...
GET /api/usage/options?...          # distinct filter values
GET /api/usage/records?...&limit=200&offset=0&sort=time&order=desc
GET /api/usage/export.csv?...
```

List filters accept repeated parameters (`key=a&key=b`). The cost column is
`$0.000000` shaped; a row without a priced model reports a zero cost rather
than a guess, and the "standard" figure shown on the card is the same estimate
expressed at full (non-cached) rates.
