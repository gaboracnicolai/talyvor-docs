# Email Notifications (Docs)

Docs can email people when something happens on a page they care about. The
whole feature is **dark by default**: with `EMAIL_ENABLED` unset, Docs behaves
byte-for-byte as it did before — no worker starts, no rows are written, no
addresses are read.

This document covers the events, defaults, configuration, delivery model, and
security posture. It is the operator-facing reference for turning the feature
on.

---

## Events

| Event                     | `event_type` key            | Who is notified (actor excluded)         |
|---------------------------|-----------------------------|------------------------------------------|
| Review requested on a page| `page.approval_requested`   | Each requested reviewer                  |
| You were @mentioned       | `page.mentioned`            | Each resolved mentioned member           |
| Your pages are going stale| `page.stale_digest`         | Page owner — one **digest** per owner    |

The person who performed the action is never emailed about their own action
(`dedupeExclude`). The stale digest is driven by the freshness scheduler and
has no actor to exclude.

---

## Recipients directory (Docs-specific)

Docs stores **no user emails of its own**. Addresses are resolved from a
dedicated `notification_recipients` directory (`member_id → email`). This table
is **populated by a separate identity-sync seam** (`RecipientStore.Upsert`);
until that seam runs, the directory is empty and **no email is sent** — a member
with no directory row is simply skipped. This is the conservative posture: the
feature ships inert in two independent ways (master switch off, directory
empty).

---

## Preferences & defaults

Per-member, per-event preferences live in `notification_preferences`. The model
is **opt-out**:

- **No row → the member receives the email** (default ON).
- A row with `email_enabled = false` suppresses that event type for that member.

> ⚠️ **Divergence from the original spec — read before flipping on.** The
> overnight spec called for a *default-OFF* posture for everything except
> mentions ("conservative against spam"). This ships **default-ON for all event
> types** (opt-out), a deliberate, test-pinned choice in the original PR. The
> directory-empty-by-default posture mitigates spam risk at launch, but once
> identity sync populates recipients the default-ON behaviour applies. See the
> filed issue for the conservative alternative.

---

## Configuration

Email is delivered over **SMTP only** (no third-party email API dependency).
SMTP settings live in **product-neutral `EMAIL_*` env vars** (the
`internal/email` package is shared across Talyvor products). Per-product config
(base URL for deep links) stays under `DOCS_*`.

| Variable            | Required (when enabled) | Default     | Notes                                   |
|---------------------|-------------------------|-------------|-----------------------------------------|
| `EMAIL_ENABLED`     | —                       | `false`     | Master switch. Off ⇒ feature fully dark.|
| `EMAIL_SMTP_HOST`   | ✅                      | —           | SMTP relay host                          |
| `EMAIL_SMTP_PORT`   |                         | `587`       | STARTTLS negotiated when advertised      |
| `EMAIL_SMTP_USER`   |                         | —           | If empty, no SMTP auth is attempted      |
| `EMAIL_SMTP_PASS`   |                         | —           | **Never logged**                         |
| `EMAIL_FROM`        | ✅                      | —           | Envelope + From address                  |
| `EMAIL_FROM_NAME`   |                         | `Talyvor`   | Display name                             |
| `DOCS_APP_BASE_URL` |                         | `localhost` | Used to build deep links in emails       |

### Enabled-but-misconfigured behaviour

If `EMAIL_ENABLED=true` but the minimum SMTP settings (`HOST` + `FROM`) are
absent, Docs **falls back to a no-op sender** (logs, sends nothing) rather than
failing startup — a deliberate fail-safe (`sender_test.go`). The overnight spec
asked for the opposite (fail startup loudly); the divergence is filed for a
decision.

---

## Delivery model

```
event seam (approval / comment / page / freshness scheduler)
   └─ Dispatcher: resolve recipients → exclude actor → dedupe
        → preference filter → resolve addresses (notification_recipients;
          members without a row are dropped)
        → render once (HTML + text)
        → Queue.Enqueue  ── never blocks the request/job ──┐
                                                            │
   in-process worker pool ◀──────────────────────────────────┘
        → SMTP send, bounded retry with backoff
        → on success: done
        → on exhausting all attempts: **dead-letter** (durable) + log
```

- **Never blocks a request/job.** `Enqueue` is non-blocking; if the bounded
  buffer is full it drops with a warning (best-effort).
- **Retry/backoff.** Each message is attempted `Attempts` times (default 3) with
  linear backoff.
- **Dead-letter.** When all attempts fail, message metadata (recipients,
  subject, attempts, last error) is recorded in `notification_dead_letters` and
  surfaced at `GET /v1/notifications/dead-letters`. The rendered body is **not**
  stored.
- **Graceful drain.** On shutdown the queue drains buffered messages within a
  bounded window.

### Known durability boundary (deferred)

A message **in flight when the process is killed** is still lost — the queue is
in-memory. Full at-least-once delivery needs a write-ahead outbox. Dead-letter
covers the common case (SMTP down long enough to exhaust retries).

---

## Templates

- One source of truth per event, rendered to **plain-text first, minimal HTML
  second** (`multipart/alternative`).
- **No remote assets, no tracking pixels.**
- Every email footer carries an unsubscribe / **manage-preferences** link.

---

## Security posture

- **HTML injection:** all user-supplied content (page titles, comment bodies,
  digest item titles) is rendered through Go's `html/template`, which
  auto-escapes. Pinned by `render_test.go`.
- **SMTP header (CRLF) injection:** header values derived from user content
  (Subject, recipients) are sanitized — CR/LF collapsed to a space — so a
  newline in a page title cannot inject a `Bcc:` or a fake body. Pinned by
  `smtp_test.go`.
- **Enumeration:** recipients are addressed only by the address the directory
  resolves for their member ID; an unresolved or empty address is dropped. The
  system never sends to a user-supplied per-send address. Pinned by
  `dispatcher_test.go`.
- **Flood / coalescing:** the conservative choice is a **bounded buffer** — under
  a burst the queue stays non-blocking and sheds load; recipients are
  de-duplicated per event, and the stale digest coalesces all of an owner's
  pages into **one** mail. (No cross-event coalescing for approvals/mentions
  yet — see deferrals.)
- **Credentials:** `EMAIL_SMTP_PASS` is never written to any log line.

---

## Deferrals (filed as issues, intentionally not built)

- Digest / batched-summary emails for approvals + mentions (the stale-page
  digest already coalesces; this extends it to the per-event notifications).
- Outbound webhooks as an alternative delivery channel.
- Per-workspace branding (logo / colors / from-name).
- Write-ahead outbox for full at-least-once delivery.
- Default-OFF (opt-in) preference posture for non-mention events.
- Fail-startup on enabled-but-misconfigured SMTP.
