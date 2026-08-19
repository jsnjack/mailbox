# IDEAS

Focused work needed to move Mailbox toward 9/10. Move an item into implementation
and remove it here when it ships.

## Encrypted backup and diagnostics

Add safe recovery and support tooling:

- Export and restore the database, configuration, signatures, and optional caches.
- Encrypt backups with a user-supplied passphrase; never silently export keyring secrets.
- Verify backup integrity and schema compatibility before replacing local data.
- Generate a redacted diagnostics bundle containing versions, provider types, sync state,
  schema/DB health, metrics, and recent logs.
- Strip message content, addresses, tokens, filesystem paths, and AI keys by default, and
  show a preview of every file before the bundle is saved.

## Per-segment translation cache

Translations are cached per message. The pooled request path already shows why a
finer key would pay: on a real five-message thread, 314 extracted segments hold
only 85 distinct ones, because every reply quotes the ones before it. Pooling
collapses that within a single translate pass, but a later pass on a neighbouring
thread — a forward of the same mail, a reply that arrives tomorrow — re-translates
text the app has already translated verbatim.

Storing `(hash(segment), lang) → translation` would make quoted history free
across threads and restarts, and the render path already looks translations up by
source text (`translationPlan.render`), so the shape fits. What needs thought is
eviction (segments are unbounded where messages are not) and whether the row count
stays sane on a large mailbox before it earns its schema.

### One request per conversation (rejected, measured)

Sending a whole conversation as a single translate request was measured against the
current shape (one bounded batch per 40 pooled segments, four in flight) on real
threads through the configured chain:

| thread | per-message, concurrent | one request per thread | pooled batches (shipped) |
| --- | --- | --- | --- |
| 3 messages, 20 segments | 3.2s | 4.4s | — |
| 4 messages, 163 segments | 6.9s | 17.1s | — |
| 5 messages, 314 segments | 13.7s | 67.5s | 7.6s |

One request cannot overlap with itself, so it degrades with thread length while
parallel batches stay flat, and a single drifted reply loses the whole conversation
rather than one batch. The win was never the request boundary: it was pooling the
duplicate quoted text (roughly a quarter of the segments survive dedup) and naming
each snippet by index so a merged reply can no longer shift translations onto the
wrong paragraph — measured 71 snippets returning as 57 with a positional array,
against 0 missing keys for the same body keyed.

## Optional community tracker list (deferred)

Keep the current structural and URL heuristics as the only tracker protection for now.
They remove tiny or concealed images, suspicious CSS resources, and recognizable
open/read endpoints before any request is made. A downloaded community list could add
useful coverage, but the expected gain does not yet justify its parser, update path, and
false-positive surface without a Mailbox-specific benchmark.

### Research and estimated value

- Englehardt, Han, and Narayanan tested EasyList + EasyPrivacy against a real email
  corpus. Request blocking reduced senders leaking an email address from 19% to 7% and
  third-party leak recipients from 99 to 51; the paper reports an 87% reduction in leak
  events overall. The lists still missed custom and first-party URL patterns. See
  [I never signed up for this!](https://www.ftc.gov/system/files/documents/public_events/1223263/privacycon_emailprivacy_englehardt_0.pdf).
- Cernera et al. evaluated more than 44,000 emails and 22 commercial tracking services.
  Depending on the heuristic, the share of messages identified as tracked varied from
  10% to 85%; small changes to dimensions, paths, or identifiers evaded detectors.
  EasyPrivacy and Proton Mail's private list each found URLs missed by the other, so
  structural detection and lists are complementary rather than interchangeable. See
  [Hard to See, Harder to Block](https://massimolamorgia.com/assets/pdf/email_tracking.pdf).
- A broader web-tracking study found EasyList + EasyPrivacy missed 25.22% of tracking
  requests identified by its behavioral detector. This is not an email-specific result,
  but it reinforces that a list cannot replace heuristics. See
  [Missed by Filter Lists](https://arxiv.org/abs/1812.01514).
- Before a Mailbox benchmark exists, the engineering estimate is 60–80% coverage for
  the current heuristics, 70–85% for EasyPrivacy alone, and 85–95% when combined. The
  likely incremental value is 10–25 percentage points, mainly for known providers using
  normal-looking dimensions, randomized paths, or recognized tracking hosts. These are
  estimates, not measured project claims.

### Proposed solution if revisited

- Add a **Use community tracker list** preference. Turning it off must make no list-update
  requests and must ignore the cached list while retaining the existing heuristics.
- Download EasyPrivacy directly from its official HTTPS endpoint in the background only
  when no valid snapshot exists or the cached snapshot is more than seven days old. Do
  not delay startup; offline and failed updates continue with the last valid snapshot or
  heuristics alone.
- Use `ETag` and `Last-Modified` validators, a strict response-size limit, bounded parsing,
  atomic cache replacement, and last-known-good rollback. Record the source, fetch time,
  list version, and validation result locally; send no telemetry.
- Implement the relevant Adblock Plus semantics for image and CSS requests, including
  exception rules and resource/domain modifiers. Do not turn the file into a naive domain
  denylist: EasyPrivacy deliberately contains broad web rules that could hide legitimate
  email artwork when applied without context.
- Show the source, last successful update, and attribution in Preferences. Keep the list
  in the XDG cache rather than distributing a snapshot with the application.
- Build a versioned test corpus containing known trackers, evasions, and legitimate
  newsletter images before enabling the feature by default. Record heuristic-only,
  list-only, and combined precision/recall; proceed only if the measured incremental
  coverage is meaningful and visible-image false positives remain acceptably low.
