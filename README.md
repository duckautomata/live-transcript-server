# live-transcript-server
A Go WebSocket server that receives transcript data from worker and then propagates it out to all web clients.

## Overview

_System_

- **[Live Transcript System](#live-transcript-system)**
- **[Server System](#server-system)**
- **[Flows](#flows)**
- **[Events](#events)**
- **[Live Detection](#live-detection)**
- **[Notification Events](#notification-events)**
- **[Media Clipping](#media-clipping)**

_Development_

- **[Tech Used](#tech-used)**
- **[Requirements](#requirements)**
- **[Running Source Code](#running-source-code)**
- **[Debugging/Logging](#debugginglogging)**

_Docker_
- **[Host Requirements](#host-requirements)**
- **[Version Guide](#version-guide)**
- **[Running with Docker](#running-with-docker)**

## System

### Live Transcript System
Live Transcript is a system that contains three programs:
- Worker: [live-transcript-worker](https://github.com/duckautomata/live-transcript-worker)
- Server: [live-transcript-server](https://github.com/duckautomata/live-transcript-server)
- Client: [live-transcript](https://github.com/duckautomata/live-transcript)

All three programs work together to transcribe a livestream for us to use in real-time.
- Worker will process a livestream, transcribe the audio, and then upload the results to the server.
- Server (this) acts as a cache layer between Worker and Client. It will store the current transcript. Once it receives a new transcript line, it will be broadcast to all connected clients.
- Client is the UI that renders the transcript for us to use.

### Server System

The server is a straightforward Go WebSocket server that has three main functions:
1. Store transcript data for any given key
2. Upon a new connection, send them the current transcript data
3. Upon a new transcript line, add the line to the local transcript and broadcast the line to every client.

Some additional functions it does is
- store media data to file and serve them upon request. Either for a specific line or merge multiple lines into one media file.

The main functions are explained in [Flows](#flows) and [Events](#events). The media data is explained in [Media Clipping](#media-clipping).

### Flows
#### From worker
Stream Start
- worker calls /{key}/activate?data...
- Server updates data with the new stream and broadcasts details to all clients

Stream is running
- Worker calls /{key}/update with the new transcript line and media.
- if the received line has the correct ID (next in line), the server adds the new line to its local data, saves the media to file, then broadcasts the new line to all clients
- else, that means the Server and Worker are out of sync. To fix this, the server responds with 409, telling the worker to call to upload, which will reset the server state to the client's current state.
    + We expect there to be missing data if the Server and Client go out of sync. But we'll let the Client figure that out on how to proceed.

Fixing out-of-sync issue
- worker calls /{key}/upload with its entire current state.
- Server resets its state with the data the worker provided.
- The server broadcasts the last line of the new transcript to every client.
    + This will cause every client to go out of sync with the server. But we're OK with this since the client will decide how to proceed. We don't want to resync every client if some are unused.

Stream ends
- worker calls /{key}/deactivate?data...
- server updates live to false and broadcasts details to all clients

#### From client
New connection
- Client calls /ws/{key}
- connection turns into a WebSocket
- hardRefresh(conn) is called, and the current state is sent to the client

Missing data
- Currently, there is no support to update any missing data

Hard Refresh
The client wants to resync the entire state.
- Currently, the only support for hard refresh is for the client to close the connection and open a new one.

### Events
#### Server to Client
- refresh to add a new line
- hardrefresh to update the entire state
- newstream when a new stream starts. Reset client state.
- status when the status of a current stream changes.
- error when there is some error with the server.

#### Message structure
All messages will start with `![]` and split up the parts of the message with `\n` except for hardrefresh, which will send the entire transcript data as a JSON for simplicity.

The goal of this message structure is to minimize data being sent between the server and the client. It also minimizes the computation time needed to generate the message. JSON is very slow.
- ![]refresh\n{id}\n{line_timestamp}\n{upload_time_ms}\n{process_start_timestamp_ms}\n{timestamp_1}\n{text_1}\n{timestamp_2}\n{text_2} ...
- ![]newstream\n{streamId}\n{streamTitle}\n{startTimeUnix}\n{mediaType}\n{isLive}
- ![]status\n{streamId}\n{streamTitle}\n{isLive}
- ![]error\n{errorType}\n{message}

### Live Detection

The server can detect for itself when a configured channel goes live, so a
stream is still noticed when the Discord bot is down or the worker misses it.
It also notices, on YouTube, a stream or premiere being scheduled and a video
or short being published.

Every observation is recorded once in a ledger and then does three things:

1. **Queues the stream for the worker** - only for a live broadcast, only
   when `liveDetect.queueIncoming` is on, and not for a members-only YouTube
   broadcast, which the worker refuses, unless `liveDetect.queueMembersOnly`
   is on too (or it is opened to everyone while live, when it is queued then).
   This is the same queue a Pingcord
   announcement picked up by the Discord bot feeds, so once detection is
   trusted the bot becomes redundant. With `queueIncoming` off, detection is an
   observer: it never queues work, never writes the `streams` table, and never
   activates anything. A new deployment runs that way first and reads the
   measured delays on the admin page before opting in.
2. **Runs the channel's notification events** - the public announcements
   accounts set up on the live-transcript site, described under
   [Notification Events](#notification-events).
3. **Mirrors it to the operator feed** (`discord.detectWebhookUrl`) in the
   default announcement look, with no pings, and with the detection
   diagnostics (which mechanism won, and the delay behind the platform's own
   start time) in the footer. The same feed carries detection problems,
   including a ping when a live broadcast is detected more than ten minutes
   after the platform's own start time (or one interval of the platform's
   slowest poll plus five minutes, when `discoverySeconds` or `pollSeconds`
   is set high enough to make that longer): healthy detections land within
   seconds, so a late one is a stream that was effectively missed. A
   broadcast the search audit rescued is not pinged again as late; the
   audit's own alert already covered it.

Disabled by default (`liveDetect.enabled`).

Six mechanisms run together, deliberately redundant (a seventh label,
`youtube-manual`, marks a claim an operator rescued with **Check video**):

| Mechanism | Platform | Kind | Latency |
| --- | --- | --- | --- |
| `twitch-eventsub` | Twitch | push (webhook) | 1-3s |
| `twitch-poll` | Twitch | poll (`helix/streams`, 60s) | ~1 min |
| `youtube-websub` | YouTube | push (PubSubHubbub) | seeds a video id in seconds |
| `youtube-state-poll` | YouTube | poll (`videos.list`, 3-300s adaptive) | seconds once the id is known |
| `youtube-discovery` | YouTube | poll (`playlistItems.list`, 120s) | floor for an unscheduled go-live |
| `youtube-search-audit` | YouTube | poll (`search.list`, 3h at fixed UTC slots) | cross-check; alerts on a miss and seeds it, and the claim that follows is recorded under it |

The split matters. Push paths give the latency but are **edge-triggered**: one
dropped delivery and the stream is never seen. Polling paths are slower but
**level-triggered** and self-healing - they re-derive the truth every cycle, so
they recover from a missed webhook, a revoked subscription, a Cloudflare block,
or a restart mid-stream. Running both means push supplies the speed and poll
supplies the guarantee.

Detection is opted into **per channel** by which identifiers a channel config
carries: `twitchLogin` alone means Twitch only, `youtubeChannelId` alone means
YouTube only, both means both, neither means the channel is not watched. Both
values are format-checked at startup - every way of getting one wrong fails
silently at the API, so a typo would otherwise read as "never goes live"
indefinitely - and the startup log prints the resulting mapping.

Two rules hold the design together:

- **The ledger claim.** Every mechanism calls `Sink.ObserveLive` on every cycle
  it sees a broadcast - hundreds of times per stream, from several goroutines.
  `Store.ClaimDetection` is an `INSERT OR IGNORE` whose `RowsAffected` decides
  who notifies, so exactly one wins. That is what makes the redundancy free,
  and the mechanism recorded on the winning row is a real measurement of which
  path is fastest. A restart needs no special case: it is a new platform
  broadcast id, so it is a new row and it notifies again.
- **"I don't know" is never "offline".** Every poll returns an error rather
  than an empty result when it could not determine liveness. Helix additionally
  needs several consecutive absences before a stream is called ended, because
  its responses are edge-cached and a successful 200 can simply omit a channel
  that is still live.

Discovery is the one cost that scales with channel count and is paid whether or
not anyone streams (up to two quota units per channel per cycle), so
`discoverySeconds` must be scaled with the number of YouTube channels; the
startup log prints the projection and warns when it leaves too little headroom
for the fast ladder.

Each discovery pass reads two playlists per channel: the public uploads
playlist and the members-only playlist (`UUMO...`). YouTube lists members-only
streams, waiting rooms, videos and shorts only in the latter - never in the
uploads playlist or the channel feed WebSub watches - so reading it is how they
are detected. Most channels have no members-only playlist; YouTube answers 404
for those, and discovery then asks only once an hour. The members-only reads
report their health as their own leg, `youtube-members-discovery`.

The optional search audit (`liveDetect.youtube.searchAudit`) is the backstop
for a broadcast detection never sees. At fixed UTC slots - 00:20, 03:20, 06:20
and every three hours after, never at startup, since restarts are frequent - it
asks `search.list` which video each channel is live on and checks each answer
against the detection ledger. A claimed broadcast (live or since ended) is
fine. One the watchlist never knew about is a discovery miss: it is alerted
immediately and seeded so the state poller still claims it. One the watchlist
was already tracking is seeded and given 30 seconds to be claimed before it is
alerted as tracked but never detected. Either way the claim the state poller
then makes is recorded under `youtube-search-audit`, not `youtube-state-poll`:
its delay measures the miss, not the poll ladder. When the ledger cannot be
read, nothing is alerted, but an id the watchlist does not know is still
seeded. A channel the ledger already holds a
live YouTube broadcast for is skipped to save its call; a live Twitch broadcast
does not count. Each pass logs what search returned and when the next slot is.
The audit is budgeted at 40 calls a day (one per channel per slot, eight slots),
on the understanding that `search.list` has its own 100-call daily bucket.
That billing is unverified - the published cost table charges it 100 units of
the shared allocation - so check the API's quota page in the Cloud Console
before raising the budget.

Latency comes from knowing the video id *before* the stream starts. Every
premiere and any stream with a waiting room is on the watchlist long in advance,
so the `upcoming -> live` transition is caught within seconds; only an
unscheduled surprise go-live waits for a discovery pass. `videos.list` costs one
quota unit per **call** regardless of how many ids it carries, which is what
makes a three-second poll affordable.

The watchlist the state poll reads is in memory, and how ids leave it decides
what can still be detected:

- **Ended** videos - finished streams, and the ordinary uploads discovery
  finds on every page - leave ten minutes after they are first seen ended and
  go into a 24-hour negative cache, so discovery does not re-seed the same back
  catalogue on every pass. They are the only thing that cache ever holds.
- **Upcoming** frames are never written off by their schedule. A frame still
  waiting hours or days past its announced start is not abandoned: creators
  hide a waiting room and re-publish it later without editing the schedule,
  then go live on it. It stays on the cold rung of the ladder (every five
  minutes, riding along on calls made anyway) and is aged out 24 hours after it
  was first seen - not negative-cached, so the next discovery pass re-seeds it
  while it is still listed. The re-seeded frame is polled straight away and
  then left to the ladder, without the ten minutes of three-second polls a
  newly found id gets, which would cost about 200 units a frame every day. Until
  2026-10-11 such a frame was negative-cached as
  "abandoned", which hid a re-published waiting room from discovery for a day;
  its stream went undetected for two hours, until a restart cleared the cache.
- An id `videos.list` **stops returning** (deleted, made private) is dropped
  after five consecutive misses, also not negative-cached, so one that comes
  back is rediscovered. The exception is a members-only id while `videos.list`
  has never answered for any members-only video: it gets a negative cache of
  its own, or it would be re-seeded and re-polled forever.

Every drop other than an ended video is logged with the video id, the channel
key, a `reason` (`aged-upcoming`, `aged-unknown`, `unreturned`), the state it
was last seen in and its schedule, and discovery's log lines name the ids they
seed (up to ten a line; the first pass after a start only counts them) - so
"was this video ever on the watchlist, and what took it off?" is one grep.

Both push callbacks are public and unauthenticated by necessity - Twitch and
Google cannot send an API key - and verify an HMAC over the raw request body:

- `POST /livedetect/twitch/eventsub`
- `GET,POST /livedetect/youtube/websub`

**Behind a CDN, exempt `/livedetect/` from bot protection.** Twitch and the
WebSub hub both send as `Go-http-client/1.1` from cloud IPs - exactly what bot
protection targets - and the failure is worse than a block: Cloudflare's AI
Labyrinth answers with a decoy page and a **2xx**, so the sender records the
notification as delivered and discards it, while nothing reaches this process to
be logged. An hourly probe checks the callback path from the outside and alerts
when anything other than this server answers it, because that is the one failure
invisible from both ends.

See `liveDetect` in `config-example.yaml` for the full setup.

Non-live observations (scheduled frames, uploads, shorts) come from the same
YouTube state poll. A video that reports no broadcast details is an ordinary
upload; one request to `youtube.com/shorts/{id}` then tells a short (200)
from a video (redirect to `/watch`), and an answer that is neither is
announced as a video - the harmless direction. Only a video published within
the last six hours is announced, so the first discovery pass after enabling
does not announce a channel's whole back catalogue.

#### Manual YouTube controls

Two admin endpoints recover a missed YouTube go-live without restarting the
server. Both take the channel's admin key (`X-Admin-Key`) and are on the admin
page under the **Live detection legs** card (Notifications tab), which offers
them when the channel is watched on YouTube (`liveDetectYouTube` in
`GET /{channel}/admin/info`). Both run on the detector's discovery goroutine,
between passes - the members-only bookkeeping they share with discovery
belongs to that goroutine alone - and a request waits at most 45 seconds for
it. Errors are JSON, `{"ok": false, "error": "...", "retryAfterSeconds": N}`.
A successful call is posted to the admin audit webhook and wakes the admin
page; a refused one is only logged.

**`POST /{channel}/admin/livedetect/check`**, body
`{"url": "<YouTube URL or video id>"}`. Accepts `watch?v=`, `youtu.be/`,
`/live/`, `/shorts/` and `/embed/` links (tracking parameters are fine) and
bare 11-character ids; a channel link such as `youtube.com/@name/live` is
refused. It looks the video up (`videos.list`, one unit), refuses it unless it
belongs to this channel's `youtubeChannelId` - otherwise one channel's admin
could have another channel's stream announced and queued as theirs - removes
it from both negative caches, reads the channel's members-only playlist (one
unit, skipped while the channel is known to have none) to tell whether it is
members-only, and puts it on the watchlist due for an immediate poll - without
the ten-minute fast-poll boost discovery gives a new id, since the check has
just read the video's state and the poll ladder handles an upcoming one. The
state poll then detects it the normal way within seconds: claimed (recorded
under `youtube-manual` when the video was already live at the check), queued
(with `queueIncoming`), announced, and flagged by the late-detection alert if
it started more than ten minutes earlier. A check costs at most three units:
the lookup, the members-only read, and the one poll it brings forward. At most
20 checks per channel per quota day; refused checks count, since each one
still cost a lookup.

| Status | Meaning |
| --- | --- |
| `200` | `{"ok": true, "videoId", "channelKey", "url", "state", "title", "scheduledStart", "actualStart", "membersOnly", "membersOnlyChecked", "unretired", "wasTracked", "tracked", "checksRemaining"}`. `state` is `upcoming`, `live`, `ended` or `unknown`; times are unix seconds; `unretired: true` means detection had wrongly written off a video that has not ended (an ended one in the negative cache is expected, and reads `false`); `membersOnlyChecked: false` means the members-only playlist was not read - the channel has none, is backed off, or the read failed - so a video not already known to be members-only is treated as public. |
| `400` | Not a video link or id, or the video belongs to another channel. |
| `404` | YouTube does not return the video: private, deleted, or a wrong id. |
| `429` | This channel's daily check cap or the unit budget is spent; `Retry-After` says until when. |
| `502` | The YouTube API call failed. |
| `503` | Detection is disabled, the YouTube leg is not running, the server is shutting down, or the channel has no `youtubeChannelId`. |
| `504` | The discovery loop did not answer in time. |

**`POST /{channel}/admin/livedetect/discover`**, no body. Runs one discovery
pass over every channel now, spending what a scheduled pass spends; the next
scheduled pass then waits a full interval. At most one manual pass a minute
across all channels, and six per channel per quota day: the spacing alone let
one channel's script double the discovery rate and spend the whole budget.

| Status | Meaning |
| --- | --- |
| `200` | `{"ok": true, "channels", "playlistsRead", "failures", "budgetRefused", "unitsSpent", "found", "foundMembersOnly", "foundOtherChannels"}`. The pass covers every channel, but `found` and `foundMembersOnly` list only this channel's new ids; other channels' finds are only counted. |
| `429` | A manual pass ran less than a minute ago, or this channel has used its six for the quota day; `Retry-After` says when the next is allowed. |
| `503` / `504` | As for `check`. |

#### Runbook: a stream is live but was not detected

1. **Start the transcript.** Queue the stream from the admin page's
   **Incoming queue** as `https://www.youtube.com/watch?v=<id>`. The worker
   picks it up in about 16 seconds, and every minute spent on the steps below
   is otherwise lost from the transcript. That form is the one detection queues
   itself, so if detection gets there while the entry is still waiting, the
   two are one entry.
2. **Let detection record it.** Paste the same URL into **Check video**.
   Detection claims the stream and runs its announcements within seconds -
   late, and the late-detection alert pings the operator feed if it started
   more than ten minutes ago. The answer's `unretired` (shown as *was hidden
   from discovery*) says whether a negative cache was hiding it although it
   had not ended.
3. **Find out why.** Search the server log (`tmp/_logs/server*.log`, JSON) for
   the video id and for the channel key (`"key":"<channel>"`). Discovery lines
   name the ids they seeded, drop lines carry a `reason` and the state and
   schedule the video was last seen in, the check logs
   `operator check seeded a youtube video` with `unretired`, and the search
   audit logs what search reported live at each slot.
4. **Do not restart the server to fix it.** A restart erases the in-memory
   watchlist and caches that explain the miss, resets the quota counters (the
   governor then believes the day's budget is unspent), and disconnects every
   viewer. Steps 1 and 2 do everything a restart would.

### Notification Events

Notification events are the audience-facing half of live detection: the
"Pingcord-like" rules anyone can set up for a channel. Each one says *when
any of these triggers fires for this channel, post this message and embed to
these Discord webhooks*.

They are managed on the live-transcript site's **Notifications** page, under
an account. Accounts exist so that everyone can run their own events and
nobody can see anyone else's: a Discord webhook URL is a credential, and an
event's webhooks belong to the account that created it alone - every read and
write is scoped to the signed-in account, and an id that is not yours is
simply not found. The admin page keeps a read-only view of every event on a
channel (owner named, webhooks masked) with a moderation delete; events from
before accounts existed show there as *legacy admin* and can only be deleted.

**Accounts** (`accounts` in the config; `/auth/*` on the API):

- Username and password; sign-ups are open unless `accounts.disableRegistration`
  is set. Passwords are argon2id-hashed (OWASP parameters), 8 to 128
  characters, and refused when they are the username or on the short list of
  passwords everyone tries first.
- Sessions are bearer tokens (`Authorization: Bearer ...`), 256 bits of
  entropy, stored only as a SHA-256 hash, sliding 30-day expiry with a
  180-day ceiling. A cookie was rejected on purpose: the API host serves the
  production and dev servers under one origin, the site is developed against
  the production API from localhost, and a bearer token carries no CSRF
  surface. Changing the password ends every other session; deleting the
  account removes its sessions, events, cooldowns and delivery log.
- An account can be shared - a whole mod team signing in at once, from
  wherever they are, is the intended use. `GET /auth/sessions` lists every
  live session (browser, when it signed in and was last active, which one is
  asking - never the address, so one member cannot see where the others
  are); `DELETE /auth/sessions/{id}` ends one of them, and
  `POST /auth/logout-all` ends every session but the caller's. Ids belong to
  the account, so another account's session is simply not found.
- There is no recovery path - no email, no reset link. The site says so
  before an account is created or a password changed, and asks for a
  checkbox that the credentials are saved. A lost password is a lost
  account; the operator can delete it so the username is free again, but
  cannot get into it.
- Sign-in is limited per address, and after a few wrong passwords the
  (username, address) pair waits a doubling interval (one minute up to an
  hour) that is forgotten once it is left alone. The wait is per address and
  per name typed - a stranger cannot lock a shared account's owners out from
  somewhere else, and an unknown username waits exactly like a real one, so
  neither the wait nor the answer says which usernames exist. A wrong
  username and a wrong password get the same answer after the same work.
  Password checks behind a session (change password, delete account) go
  through the same limits, and the number of password hashes in flight is
  capped process-wide so a flood cannot take the box's memory. Behind a
  proxy, set `accounts.trustedProxies` so the forwarded client address is
  believed only from it. A lock always expires on its own (the wait is at
  most an hour, and the failure count is forgotten after an hour of quiet);
  the operator can also clear it from the site admin page, and a restart
  clears every lock since they live in memory.
- What accounts do with their own events (create, edit, pause, delete, test
  send) is written to the server log with the account named and the webhooks
  masked, but not to the admin audit webhook: that log is for operator
  actions, and these are open to every account.

**Site admin** (`credentials.adminKey`; `/admin/ui` and `/admin/*`): the
operator's page over the whole site, separate from the per-channel admin
pages and gated by its own key (wrong keys are throttled per address; no key
configured turns the page off). It shows every account with its event,
webhook and session counts, the channels it posts on, when it signed up, last
signed in and was last active, and its last delivery error; each account's
events can be expanded (webhooks masked, as everywhere outside the owner's
editor) and deleted one by one. Per account it can **unlock** (forget every
address's failed attempts against the username), **sign out everywhere**,
**disable** with a reason (sign-in and every session are refused with the
reason, the account's events stop firing but are kept, and the channel admin
pages label them *(disabled)*), **enable**, **delete events** and **delete the
account**. Site-wide it shows totals, recent sign-ups, the last failed or
partial deliveries across every account, live detection's legs, and can
close and reopen sign-ups at runtime (`accounts.disableRegistration` in the
config closes them regardless). Every action is posted to the admin audit
webhook and logged. The channel names `admin`, `auth`, `status`, `events`,
`livedetect`, `metrics`, `health` and `version` are reserved for these routes
and refused by config validation.

- **Triggers:** going live (Twitch stream, YouTube stream, or YouTube premiere
  starting), stream or premiere scheduled, video uploaded, short uploaded.
- **Webhooks:** one or many Discord webhooks, each with a name saying where
  it posts. Only Discord webhook URLs are accepted, so a rule can never point
  the server at an arbitrary host. The URLs are shown in full only in the
  editor; every log line, error and audit record uses the name and a masked
  form of the URL.
- **Message and embed:** templates with `{placeholders}` (`{channel}`,
  `{title}`, `{url}`, `{time}`, `{timeShort}` for just the clock time, ...).
  `{description}` is the YouTube video's description in three forms:
  `{description}` is all of it, `{description:1}` the first line, and
  `{description:3}` the first 3 lines of text (any count from 1 to 99; blank
  lines don't count). It is blank on Twitch, which has no descriptions.
  `{game}` is the other way round: the Twitch category being streamed
  (Minecraft, Just Chatting), blank on YouTube. A line whose placeholders
  all came up blank, and that has no words left on it (an emoji or a quote
  mark does not count), is removed, so the same template reads well on both
  platforms; and when a message would be too long for Discord, the
  description is what gets shortened, not the text written around it. In the
  message text the description's links are written as `<https://...>`, so
  Discord does not add a preview card for each one. Discord also reads a `!`
  or `?` straight after a bare link as part of the address, so a link
  placeholder written right before one (`live at {url}!`) is sent the same
  way, ending where the link does; leave a space after it to keep the
  preview card. A scheduled announcement
  carries the description as it read when the waiting room was first seen,
  while the go-live announcement carries the text at go-live; the detection
  ledger is write-once, so later edits to a description are not picked up.
  Pings go in the message as `<@&ROLE_ID>` for a role, `<@USER_ID>` for one
  person, and `<#CHANNEL_ID>` for a channel link that pings nobody; the
  editor has a helper that inserts each and explains where the ids come from.
  The embed starts out looking like the server's own stream-start post and
  can be edited or disabled.
- **Minimum time between notifications:** a per-rule, per-trigger cooldown
  claimed atomically in the database, so a stream restart (a brand-new
  broadcast id) cannot ping everyone twice, while a "scheduled" ping never
  swallows the "live" ping that follows it. Suppressed sends are logged.
- **Mentions:** the pings a message can make are derived from the template,
  never from the rendered text - a stream title containing `@everyone` is
  shown but notifies nobody.
- **Preview and test:** the editor renders the draft server-side from the
  channel's own most recent detection for the trigger - anything it lacked
  is left blank - and can post it to a webhook of the account's
  choice with every mention suppressed. The one labelled exception is an
  offline Twitch stream, which is previewed as it will look live (Twitch has
  no preview frame for an offline channel) with the example image, and the
  example title if none was recorded, marked as such; a test send of it goes
  out as the detection stands. Only a hand-built local binary (no `VERSION`
  set) falls back to a stand-in video when nothing has been detected; a
  deployment previews blank details instead.

These webhooks reach thousands of people, so nothing but a rendered
announcement of a real observation - or an explicit test by the account that
owns the event - is ever posted through them. Detection errors and diagnostics go to the operator
webhooks in `discord.*`, never here.

### Media Clipping

Because we don't know what type of media the worker will send to us (MPEG-TS audio, DASH video, etc.), the server treats the media received from the worker as untrusted binary data (`.raw`) and uses the `mediaType` variable to denote what type of media it is. It can be
- none if we are not sending any data
- audio
- video

When the stream starts, the worker will tell the server what media type it will send for that stream. Once mediaType is set, it cannot change for the entirety of that stream.
When the server receives new media data, it will
1. save the untrusted data as a `.raw` file under tmp/{key}/media using the line id as the file name.
2. use FFmpeg to extract the audio from it and save it in a `.mp3` using the same line ID as the file name.
3. If FFmpeg fails to extract the audio, the server treats the untrusted data as corrupted and deletes it.

When the client requests the audio for a specific line, the server will use the `.mp3` file, which is guaranteed to be a valid audio file.

#### Clipping
When the client requests a clip (either audio or video) between and including two id's, the server will
1. merge all `.raw` files in that range into a single `.raw` file
    + Because this is the unmodified stream data, doing it this way ensures there are no gaps between lines. You would get gaps if you tried to merge the `.mp3` files since the conversion is not perfect.
2. use FFmpeg to convert the `.raw` file into the requested media type file (either `.mp3` for audio or `.mp4` for video)
3. delete the merged `.raw` file and respond with that new file. Which is guaranteed to be a valid media file.

## Development

### Code Layout

The module is split into focused packages so each kind of change has one
obvious home:

| Package | Purpose |
| --- | --- |
| `cmd/web` | Server entrypoint: config/logging/DB wiring, `/healthcheck`, `/version`, `/metrics` |
| `cmd/migrate`, `cmd/r2-cleanup`, `cmd/perf-test` | Operational tools |
| `internal/server` | The application core: routes, HTTP handlers (grouped worker/admin/public), stream lifecycle, maintenance loops, admin UI |
| `internal/store` | All SQLite persistence (schema, queries, transactions) |
| `internal/storage` | Media blob storage backends (local disk, R2) and storage-key builders |
| `internal/ws` | WebSocket hub: connection registry, broadcast, event payloads |
| `internal/notify` | Long-poll signaling shared by `/events` and the admin poll |
| `internal/media` | ffmpeg processing (`Processor` interface) and raw-audio merging |
| `internal/discord` | Operator webhook notifier (alerts, admin audit) + Pingcord listener bot |
| `internal/announce` | Public Discord announcements: notification-event templates, rendering, webhook delivery with cooldowns |
| `internal/livedetect` | Live detection: watches YouTube and Twitch for channels going live, scheduling streams, and publishing videos |
| `internal/archive` | Archive-server client for membership keys |
| `internal/config`, `internal/model`, `internal/metrics`, `internal/logging` | Leaf packages: config schema, shared data types, Prometheus metrics (single registration point), slog setup |

Rules of thumb for extending it:
- **New endpoint** → a handler in the matching `internal/server/handlers_*.go` file plus one line in `routes.go` (use `withChannel` / `withAdminChannel` for `/{channel}/...` routes).
- **New admin mutation** → after the write succeeds, `app.bumpAdminChange` to wake the long polls and `app.notifyAdminAction` to record it in the Discord admin audit log (read-only admin endpoints deliberately do neither).
- **New WebSocket event** → a constant + payload struct in `internal/ws/events.go`, then `Hub.Broadcast` at the emitting site.
- **New table or query** → `internal/store` only; handlers never see SQL.
- **New integration** (Slack, etc.) → a new package like `internal/discord`, depending on small interfaces the server implements (see `discord.StreamSink`), wired in `server.NewApp`.
- **New storage backend** → one file in `internal/storage` plus a case in its `New` factory.

### Tech Used
- Go 1.26
- FFmpeg

### Requirements
- [Go](https://go.dev/doc/install)
- FFmpeg
- Any OS

### Running Source Code

**NOTE**: This is only required to run the source code. If you only want to run it and not develop it, then check out the [Docker seciton](#docker)

1. Download and install Go and FFmpeg
2. Referencing `config-example.yaml`, create `config.yaml` and add your specific configurations.
5. Download dependencies `go mod download`

When all of that is done, you can run `scripts/run.sh` (or just `go run ./cmd/web/` from the root directory) to start live-transcript-server.

### Debugging/Logging

Logging is set up for the entire program, and everything should be logged. The console prints info and higher logs (everything but debug). Every log is also written as JSON to `tmp/_logs/server.log`, which rotates once it reaches 1MB; up to 10 timestamped backups (e.g. `server-<time>.log`) are kept for 90 days, uncompressed so they stay greppable on disk. Each run is bracketed by `========== SERVER START ==========` and `========== SERVER STOP ==========` banner lines so you can quickly find run boundaries. In the event of an error, check `server.log` (or the rotated backups) to see what went wrong.


## Docker

### Host Requirements
- Any OS
- Docker

If it has Docker, it can run this.

### Version Guide
Uses an x.y major.minor version standard.

Major version is used to denote any API/breaking changes.

Minor version is used to denote any code/dependency changes that do not break anything.

Tags:
- `latest` will always be the most recent image.
- `x` will be the latest x major version image. Meaning, if the tag is `2` and the latest `2.y` image is `2.10`, then `2` will use the `2.10` image. When a new `2.11` image is created, then the tag `2` will use that new image.
- `x.y` will be a specific image.

The major version between Worker and Server _should_ remain consistent.

You can view all tags on [Dockerhub](https://hub.docker.com/r/duckautomata/live-transcript-server/tags)

### Running with Docker
The easiest way to run the docker image is to
1. clone this repo locally
2. create `config.yaml` from the example config file, adding in your specific configurations.
3. then run `./docker/start.sh`

If there are permission errors and the container cannot write to tmp/, then you first need to run `sudo chmod -R 777 tmp` to give the container permissions.

Depending on your use case, you can change the configuration variables in `start.sh` to match your needs.

Logs and current state are stored in the `tmp/` folder outside the container. Because of this, state is not lost on restart.

**Note**: the docker container and the source code use the same `tmp/` folder to store runtime data. Because of this, you are required to run either or, but not both. If you want to run both development and a docker image, then use separate folders.

### Viewing Metrics
1. Go to `http://<servers ip address>:8090` to view the Prometheus webpage.
    - Click on Status and go to Targets to verify that the server target is working and up.
    - If it is not, then that means Prometheus cannot reach the server.
    - To fix this, edit the target in the `prometheus.yaml` file and make sure it has a reachable ip address.
2. Go to `http://<servers ip address>:3000` to view the Grafana webpage. 
    - Log in using admin/admin and reset the password.
    - Here, you can add Prometheus as a datasource and start creating a dashboard. Make sure to use the `8090` port address.

Important to note that Prometheus's data will reset every time you stop/start it. And Grafana's data/dashboards will reset if you run `./cleanup.sh`. So make sure that you have the your dashboards back up.
