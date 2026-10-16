# Features

## Federation
- WebFinger (`/.well-known/webfinger`), host-meta, NodeInfo 2.0/2.1, a minimal `/api/v1/instance`
- ActivityPub client-to-server: with an OAuth token (`read`/`write`, from `/oauth/token`; the actor's
  `endpoints` advertise both OAuth endpoints) a client can POST `Create`/bare `Note`, `Update`, `Delete`,
  `Like`, `Announce`, `Follow`, `Block` and `Undo` to the user's outbox (`201` with `Location`), upload
  media to `endpoints.uploadMedia` (multipart `file` + `object`, then attach it by URL), and read their
  inbox (home timeline as activities). Addressing picks the visibility; addressed or tagged actors
  are mentioned.
- Actors with RSA keys; HTTP Signatures (draft-cavage, `rsa-sha256`/`hs2019`) on every outgoing request,
  verified on every incoming inbox POST (digest, date window, key ownership, re-fetch on key rotation).
  RFC 9421 HTTP Message Signatures (Fedify-based servers such as Hollo and Ghost) are verified too, and
  deliveries and signed fetches to a host that has sent us a valid RFC 9421 signature are signed with
  RFC 9421 (`@method @target-uri @authority date`, plus `content-type content-digest` on POSTs). If that
  host answers 401/403 we retry with draft-cavage and stick to it for 30 days (double-knocking, as
  Fedify does).
- A signed instance actor (`/actor`) for fetches, so servers in "authorized fetch" mode work
- Inbox (shared and per-user) handling `Follow`, `Accept`, `Reject`, `Undo`, `Create`, `Update`, `Delete`,
  `Like`, `Announce`, `Block`
- Outbox, followers/following and featured collections, and dereferenceable statuses with
  visibility enforced via the requester's signature
- Persistent delivery queue with exponential backoff (about 3 days of retries)
- Only stores what's relevant: posts by people someone here follows, posts that mention or address local
  users, and replies to known posts. Thread parents are fetched on demand, and recent posts are backfilled
  from the outbox when you follow someone or open their profile. Opening a remote thread (signed in, web or
  `/api/v1/statuses/:id/context`) fetches the replies its server lists in the `replies` collection in the
  background, like Mastodon 4.4: three levels deep, at most 60 posts, once per thread every 15 minutes; each reply
  comes from its own origin and is only kept if it answers the post it was listed under. While it runs, the
  context API sends Mastodon 4.4's `Mastodon-Async-Refresh` header and `/api/v1_alpha/async_refreshes/:id` reports it
- SSRF protection: remote fetches never connect to loopback/private/link-local addresses
- Remote HTML is sanitized (bluemonday); custom emoji, polls (read-only), content warnings,
  sensitive media, Articles and other object types are displayed
- Groups: Lemmy communities and similar groups that relay posts as `Announce{Create}` show up as boosts by
  the group; relayed activities are always re-fetched from their origin. PeerTube videos (channel +
  account authors, Markdown descriptions, thumbnails) and Lemmy link posts display properly.
- Follow requests to remote accounts that are still unanswered after three minutes are sent once more under a
  new id (Lemmy 0.19 loses the Accept for the first follow from a server it has never seen)
- Tested for compatibility ([development.md](development.md#tests)) with Mastodon, GoToSocial, Misskey/Sharkey, Pleroma/Akkoma,
  Pixelfed, Friendica, WordPress, Fedify, Lemmy and PeerTube payloads and signature styles

## Mastodon client API
- OAuth 2: app registration, authorization code with PKCE (consent page, out-of-band codes), client
  credentials and password grants, revocation, `/.well-known/oauth-authorization-server`; connected apps
  can be revoked in Settings
- Reports itself as Mastodon 4.5.0 with `api_versions.mastodon` 7: everything the Mastodon docs list up to
  API version 7 is there (quote posts with policies, featured accounts, attribution domains, blur filters,
  media deletion, edit notifications)
- Accounts (also several at once), statuses (post with media/polls/quotes, edit, delete keeping media for
  redraft unless `delete_media`, several at once, context, source, history), favourites,
  reblogs, bookmarks, pins, reactions (Pleroma-style), polls, media upload (v1/v2), home/public/hashtag
  timelines, notifications (with type filters, plus Mastodon 4.3 grouped notifications at
  `/api/v2/notifications`: likes/boosts per post and follows per day collapse into groups), relationships, follow/block/mute, follow requests, search v2
  with remote resolving, markers, filters (v1 and v2: titles, keywords, contexts, hide/warn/blur, expiry), conversations (direct-message threads with unread state, mark
  read, hide), reports (with forwarding), quote settings (`quote_approval`, `quote_approval_policy`,
  `interaction_policy`, revoking quotes, default policy), endorsements (featured accounts),
  `attribution_domains`, instance v1/v2, CORS
  for browser clients
- Mastodon-style pagination (`max_id`/`since_id`/`min_id`, `Link` headers) and Idempotency-Key
- Streaming API over WebSocket (`/api/v1/streaming`) and server-sent events (`/api/v1/streaming/user`, ...)
- Web Push (`/api/v1/push/subscription`, VAPID, RFC 8291 encryption) so mobile and browser apps get
  notifications while closed
- Checked with the Mastodon.py client library, the toot CLI, the iOS apps Ice Cubes and Mastodon
  (their own networking/model code built on Linux), Phanpy and Elk in a browser and the Android app
  Tusky 33.1 in an emulator; see [development.md](development.md#client-tests)

## Web interface
- Home, local and federated timelines, hashtag timelines (headed by this week's posts and people, today's posts
  and a sparkline), threads with nested replies
- Autocomplete while writing: `@accounts` (people you follow first), `#hashtags` and `:custom_emoji:`, picked with
  the arrow keys and Enter/Tab or the mouse
- A reminder before posting images without a description (alt text)
- Compose with content warnings, four visibilities (public, unlisted, followers-only, direct),
  up to 4 images, videos or audio files with alt text (one "Attach media" button, drag and drop or paste; each
  file shows as a tile with its preview, alt text and a remove button; four plain file fields without JavaScript),
  @mentions (resolved via WebFinger) and #hashtags
- **AI writing (optional):** "Write with AI" in the compose box drafts a post from a short instruction,
  improves the current draft, or drafts a reply with the thread as context. The draft streams into the
  text box and is never posted automatically. Uses DeepSeek's chat completions API (any
  OpenAI-compatible endpoint works); remote post text is passed to the model as delimited data, not
  instructions. 30 requests per user and hour; admins switch it on and off and pick the model on `/admin`.
  The same model translates posts for Mastodon apps (`POST /api/v1/statuses/:id/translate`, cached per
  post version and language; the instance advertises translation while AI is on).
- "Translate" in a post's menu (with AI on): the English translation replaces the text in place, with a way back;
  without JavaScript the post's page opens translated
- Private notes on other people's profiles (also `POST /api/v1/accounts/:id/note` for apps)
- A Favourites page (`/favourites`) with the posts you starred, like Mastodon's
- A Direct messages page (`/conversations`): your DM threads with their participants, the last message and
  unread state, a compose box set to "Mentioned people only"; opening one marks it read, Hide hides it until a
  new message arrives (the same state as `/api/v1/conversations`); the sidebar's Messages link counts unread threads
- A post's boost and favourite counts (under the post on its own page) open the lists of who boosted or
  favourited it (`/s/:id/boosts`, `/s/:id/favourites`)
- Edit your posts (with an edit history page that marks the words added and removed in each edit), pin up to 5 to your profile (featured collection, `Add`/`Remove`), bookmarks
- Polls (2-4 options, single or multiple choice) federated as `Question`; vote in remote polls; poll results
  and end notifications
- Quote posts (FEP-044f `quote`, `quoteUrl`, `_misskey_quote`, FEP-e232 links) shown as embedded posts,
  with consent as Mastodon 4.5+ does it (FEP-044f): posts publish `interactionPolicy.canQuote` (anyone,
  followers or only the author; a per-account default and per post), `QuoteRequest`s for our posts are
  answered with `Accept` + a `QuoteAuthorization` stamp (served under `/users/<name>/quote_authorizations/`)
  or `Reject`, quoting a post from such a server asks it first and stays pending until approved, and
  approvals can be revoked either way. Quotes from software without the handshake work as before.
  Edits notify those who boosted (`update`) or quoted (`quoted_update`) a post;
  emoji reactions (Misskey reactions, `EmojiReact`) shown as chips, and you can react yourself
- Scheduled posts (compose box "Schedule", a Scheduled page, and `scheduled_at` / `/api/v1/scheduled_statuses`)
- Link previews (OpenGraph cards), fetched in the background
- RSS feeds of local profiles (`/@name.rss`, public posts without replies and boosts) and hashtags
  (`/tags/name.rss`), with media as `media:content`; pages link them for feed readers
- Profile directory (`/directory`, `/api/v1/directory` with `order=active|new`, `local`, paging) of accounts
  that opted in (Settings → "List me in the profile directory", the actor's `discoverable`; remote accounts
  keep their own server's flag)
- Who to follow (Explore → People, `/api/v2/suggestions` with dismissing): accounts followed by people you
  follow, then popular ones here; profiles show which people you follow follow them
- Hiding your social graph (Settings, `hide_collections` in the API): others see only the follower and follow
  counts; the lists, ActivityPub collection pages, familiar followers and suggestions don't reveal them
  (`/api/v1/accounts/familiar_followers`)
- Explore page and Mastodon trends API (`/api/v1/trends/{tags,statuses,links}`): posts with the most
  likes, boosts and replies from the last three days, and hashtags and links used by at least two people
  this week, with per-day history (also in every hashtag entity). A trending link opens `/links?url=`, the
  public posts sharing it (Mastodon 4.3's link timeline, also `/api/v1/timelines/link`)
- Custom emoji uploaded by admins, usable as `:shortcode:` in posts, names and bios, federated as `Emoji` tags
- Featured hashtags on profiles (up to 10, with post counts, linking to `/@name/tagged/tag`; "Feature on
  profile" on hashtag pages, `/api/v1/featured_tags`, `/api/v1/tags/:name/feature`), federated as the
  actor's `featuredTags` collection like Mastodon's
- A profile "Featured" tab (`?tab=featured`, shown when something is featured) listing featured hashtags
  with post counts and the accounts the user recommends
- Featured accounts on profiles (Mastodon endorsements, not federated, like on Mastodon); attribution
  domains (`attributionDomains`) for websites that credit you with `fediverse:creator`
- Blocking whole servers for yourself (Settings, a profile's "Block server" button, `/api/v1/domain_blocks`):
  hides their posts, boosts of them and notifications, removes followers there and refuses new ones;
  the list of servers the instance blocks is public (`/api/v1/instance/domain_blocks`)
- Per-follow options (profile bell and "…" menu, API `reblogs`/`notify` on follow, CSV columns): get a
  `status` notification (and web push) when someone posts, or hide their boosts from the home timeline
- Muting a conversation (post menu, `POST /api/v1/statuses/:id/mute`): no more notifications about replies, likes
  or boosts in that thread
- Mutes (indefinitely or for 1 hour/1 day/7 days, optionally posts only; API `duration` and `notifications`,
  `mute_expires_at` in `/api/v1/mutes`), keyword filters (collapse, hide media or hide; through the API also grouped, per context and expiring), blocks; reports with optional anonymous forwarding (`Flag`)
  and incoming reports for admins. Admins review them on `/admin/reports` with the reported posts shown in full
  and how often the account was reported before; resolving can delete the reported posts (federated for local
  ones), suspend a remote account (reversible; posts hidden, its activities ignored, follows end) or block its
  server, with a note for other admins. Resolved reports keep who did what.
- Suspending local accounts (from a report, the user list on `/admin` or `trough user suspend <name> on|off`):
  login and tokens stop working, profile and posts are hidden, nothing is deleted. Like Mastodon, other servers
  get an actor `Update` with `"suspended": true` and an empty profile, and another one when it's lifted.
  Accounts that their own server suspends that way are hidden here too, until that server lifts it
  (suspensions by our admins stay).
- Archive download (Settings, once an hour): a zip in Mastodon's archive layout with actor.json, outbox.json (all
  your posts and boosts, private ones included), likes.json, bookmarks.json, avatar, header and uploaded media
- Import and export (Settings) of follows, lists, blocks, mutes, blocked servers and bookmarks as CSV files in
  Mastodon's formats; imports merge and run in the background with progress shown in Settings
- Account migration: aliases (`alsoKnownAs`), moving followers to a new account (`Move`), and following
  accounts that moved here
- Sign-ups: closed, invite links or open (admin setting); live "new posts" banner and notification badge
- Lists (with replies policy and "hide from Home"), followed hashtags (public posts with them show up in
  Home; managed on `/followed_tags` with this week's activity, last use and a profile-feature toggle), and relay subscriptions (Mastodon `/inbox` style and LitePub actor style) on `/admin` to fill the
  federated timeline; relayed posts are always fetched from their origin
- Like, boost, reply and delete; likes and boosts update in place with JS, or via plain forms without it
- Following and interacting from other servers: visitors enter their own address on `/interact` (the Follow button
  on local profiles, Reply on posts) and are sent to their server's subscribe page (WebFinger's OStatus subscribe
  template, else Mastodon's `/authorize_interaction`)
- Profiles with header, avatar, bio, profile fields, locked accounts and bot flag; follow, unfollow and block
- Verified profile links like Mastodon's: a field that is a link gets a check mark (and `verified_at` in the API)
  once the page links back with `rel="me"`. Checked in the background when a local user saves the profile and
  when a remote actor's field changes; a remote actor's own `url` only counts on its own host
- Notification filters (Mastodon 4.3 policy: people you don't follow, who don't follow you, new accounts, private
  mentions from strangers; each show, filter or ignore; stranger DMs are filtered by default): filtered
  notifications wait as per-account requests to accept (which lets that account through) or dismiss, on the web
  and through `/api/v1/notifications/requests` and `/api/v2/notifications/policy`
- Notifications (mentions, replies, likes, boosts, follows, follow requests), follow request management
- Search by `@user@server`, by profile/post URL, by `#tag`, or full text over known posts
- Admin page: create users, reset passwords, instance title/description, domain blocks (including
  subdomains), federation stats and failing deliveries, and the last 7 days (new and active users, local posts,
  likes/boosts/replies on them, newly seen remote accounts, the servers most posts came from)
- Server rules (on About and sign-up, `/api/v1/instance/rules`, in the instance entity) and announcements
  (a banner until each user dismisses it; `/api/v1/announcements` with dismissing and emoji reactions)
- Video and audio uploads (web, Mastodon API, C2S): MP4/MOV, WebM, MP3, Ogg, WAV and FLAC up to 40 MB, recognised
  by their file signature and served with range requests. There is no transcoding, so only formats browsers play
  directly are taken; MP4 metadata boxes (`udta`/`meta`, where phones put the recording location) are blanked in
  place. Avatars and headers stay images only. Videos and audio can have a poster image (`thumbnail` on
  `POST /api/v2/media` and `PUT /api/v1/media/:id`, re-encoded like images, federated as the Document's `icon`;
  incoming Mastodon thumbnails are shown too). The web UI takes the poster from a frame of the chosen video in
  the browser, so the server still needs no ffmpeg.
- Focal points for images like Mastodon's: `focus` ("x,y", -1..1) on upload and `PUT /api/v1/media/:id`,
  `meta.focus` in the API, federated as the Document's `focalPoint` both ways. The web compose form sets it by
  clicking the image's tile, and cropped images in timelines (2–4 per post) keep that point in view
- Editing a post can change its media's alt text and focal point: in the web edit form (the media as tiles) and
  through the API (`media_attributes[]` with `id`, `description`, `focus`, as JSON or form fields, like Mastodon
  4.1); the Update federates the new values
- Media: image uploads are re-encoded, which strips EXIF metadata such as GPS positions while keeping the
  orientation. Remote media goes through an HMAC-signed caching proxy, so remote servers never see visitors'
  IP addresses and the page can use a strict CSP (`img-src 'self'`). Cache entries with missing data
  or invalid metadata are refetched; per-URL locks coordinate fetches and expiry cleanup.
- Search operators (web search and `/api/v2/search`, which also takes `account_id` and `max_id`): `"phrases"`,
  `-word`, `from:me`/`from:user@host`, `has:media|image|video|audio|poll|link`, `is:reply|sensitive`, `language:`,
  `before:`/`after:`/`during:` dates and `in:library` (posts you wrote, favourited, boosted, bookmarked or were
  mentioned in); any operator can be negated with `-`. Text matches content, content warnings and media
  descriptions. Without `in:library` only public posts and your own are searched.
- Drafts: unsent text, content warning and visibility are saved in the browser as you type (per account, and
  separately for a new post, each reply and each quote) and offered again with a "Discard" link; a draft is
  forgotten once the post went through. Nothing is stored on the server.
- Post language in the web compose form (44 languages by their own names): it starts with the language of
  your last post, or the browser's; used by search (`language:`), translation and other servers
- Grouped notifications in the web UI too: likes and boosts of the same post and the day's follows show as
  one entry ("ann, ben and 3 others favourited your post") with a row of avatars
- Keyboard shortcuts: j/k move between posts, o opens, r replies, f favourites, b boosts, n writes, / searches,
  ? shows the list
- Image viewer: attached images open large with their description; arrow keys go through a post's images
- Browser notifications (Settings, per browser): the web UI subscribes through the browser's push service with the
  server's VAPID key; a service worker at `/sw.js` shows them and opens the post when clicked
- Automated post deletion (Settings, like Mastodon's): your posts older than 1 week to 2 years are deleted
  (federated, 20 every ten minutes; old boosts are undone), except pinned posts, DMs, posts you favourited or
  bookmarked, polls, posts with media or posts favourited/boosted at least N times, as chosen; the card says
  how many posts are due
- Posting and reading preferences (Settings, like Mastodon's): default visibility (replies stay at least as
  private as their parent), default language, always marking media as sensitive, showing or hiding all media
  and expanding content warnings; clients see them in `/api/v1/preferences` and the credential account's
  `source`, set them through `update_credentials` `source[...]`, and posts without those params use them
- Appearance settings: theme (like the device, light or dark) and a larger text size
- Light and dark theme, responsive layout with a bottom navigation bar on phones
- Deleting your own account (Settings; password, 2FA code and username to confirm): removes posts, media and
  everything else, sends a `Delete` of the actor to followers' and followees' servers; the actor then answers
  410 Gone and the username stays reserved
- Two-factor authentication (Settings): TOTP codes from any authenticator app (QR code or key), ten one-time
  recovery codes; the web login (also used by apps through OAuth) asks for a code, the password grant is refused
- Security: bcrypt passwords, login rate limiting, HttpOnly/SameSite session cookies, per-session CSRF
  tokens, Origin checks, strict Content-Security-Policy (no inline scripts or styles)

## Not implemented

ActivityPub C2S activities other than the ones listed above.
