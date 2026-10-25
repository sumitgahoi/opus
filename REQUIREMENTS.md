# Local-first music system

## Purpose

Build a personal music server in Go with SQLite. Reliability,
simplicity, and maintainability matter more than features.

Clients read the catalog, send user actions, and keep their own
playback, offline caches, and search indexes. Those are not this
server's job.

## Scale

Worst case across the music-* system is about **one million songs**.
Design for that: indexed inventory and log reads, bounded pages,
no full-library walks on request paths, and sync/bootstrap that stay
responsive at that size. Prefer the path that stays fluent at 1M over
the path that is only fine for a few thousand.

## Architecture decisions

- Server: Go and SQLite, developed on a Debian Linux VM.
- A song is its song_id. Paths are not sent to devices. Hash identifies
the current bytes, with the in-place update in Library and identity.
- Catalog changes and user actions share one append-only log.
A device keeps one cursor.
- Clients work locally and synchronize asynchronously.
- The server durably stores accepted actions and canonical state.
- A device hydrates from one consistent read, then applies actions
after that cursor.
- Keep the SQLite database on local disk.
- The server does not search, play audio, cache songs for offline
playback, or ship a native client.

## Product requirements

### Library and identity

- Renaming or moving a file inside the root keeps its identity and
user state when the bytes are unchanged.
- Identical copies at different paths are the same asset, still found
at each path.
- A byte change at a path the song already occupies keeps the song
and updates its hash, when those bytes are not already another song
and the previous bytes are no longer in the library.
- A missing file is reported missing. Its row and history stay.
- Rescanning an unchanged library adds nothing.
- Read music files without modifying their contents or tags.
- A scan keeps MP3, AAC, Apple Lossless, FLAC, WAV, AIFF, and Dolby
Digital. Other files are left out.

MVP limitations, deferred to a later milestone:

- A move and a byte change between two scans, including a retag while
moving, are a new song. The old song stays missing and keeps its history.
- Replacing a file with bytes that already belong to another song
follows that song. The previous song becomes missing when it has no
copy left.
- Retagging one copy, while another copy of the old bytes remains,
splits that path into a new song.
- When one scan makes several songs newly byte-identical, or retags
several copies of one song into different bytes, the old songs stay
missing and new songs are created.

### User state

- Preserve plays, finishes, skips, likes, and playlists across device
changes.
- Keep each user's state isolated.
- Record offline changes in a durable local queue of actions.
- Give actions stable IDs so retries cannot apply them twice.
- Preserve distinct legitimate plays, including simultaneous plays
from different devices.
- A play counts when the listener has played the song for more than
30 seconds. Spotify counts a stream at 30 seconds or more. Apple
records a play at more than 30 seconds. More than 30 seconds is both.
A second action id is a second play. listenedMs is required and is
added to listened_ms. A song of 30 seconds or less never becomes a
play, including when it is heard through. occurred_at_ms is
descriptive and does not order anything.
- A finish counts when playback reaches the natural end of the song.
Spotify records that end as trackdone. Apple records it as
NATURAL_END_OF_TRACK. Each accepted finish adds one to finished_count.
- A skip counts when the listener skips forward to the next song.
Spotify records that as a skip to the next song. Apple records a
forward skip. Stopping, going back, scrubbing, or choosing a
different song is not a skip. Each accepted skip adds one to
skip_count.
- A play, a finish, and a skip are separate actions. Hearing the end
after 30 seconds is a play and a finish. Skipping forward after 30
seconds is a play and a skip. Skipping forward before that is only a
skip. One action does not imply the others.
- liked is the heart: Spotify's like and Apple's love.
- Every song in the catalog is already in the user's library.
- Like and unlike fold in server accept order. That order is the only
conflict rule. Client clocks are descriptive. Define playlist conflict
behavior before implementing playlists.
- Liked songs are an ordered list, not an analytics field, most
recently liked first. A like adds the song at the top. A like for a
song already liked changes nothing, so it keeps its place. An unlike
removes it, and a later like adds it at the top again. Bootstrap sends
that list as liked, in that order, separate from per-song analytics.
- Acknowledge events only after durable server persistence.
- Freeze these action types and payloads for launch. Catalog:
song_upsert `{hash, available, metadata, artwork?}`, song_unavailable
`{"available": false}`. Listening: play `{"listenedMs": N}` with
N > 30000, finish `{}`, skip `{}`. Membership: like `{}`, unlike `{}`.
Do not overload them. Playlists get new types after their conflict
rules are written. Analytics is per-user counters only
(play_count, finished_count, skip_count, listened_ms,
last_played_at_ms), folded from the log for bootstrap. Recently played
is not a table; it is a query over play actions when that screen
exists.

### Authentication

- Google OAuth 2 is the only sign-in.
- A configuration file lists the Google accounts that may use the
server. A signed-in account that is not on the list is refused.
- The accepted Google account is the user id. A user id in an action
body is not that binding.
- The Google client credentials are separate from the allowlist and
are not written to logs.

### Synchronization and recovery

- Support retries after timeouts, disconnects, and process restarts.
- One server sequence orders catalog changes and user actions.
- Commit action acceptance, derived state, and ordering consistently.
- Advance the client cursor only with the actions it has stored.
- Bootstrap reads the catalog and that user's analytics in one
database snapshot, at one seq, then applies later actions. That read
must not double-count a play already included in analytics, and must
not miss a song added after it.
- Preserve pending local actions during bootstrap or recovery.
- A successful retry must return an unambiguous acknowledgement.
- A cursor past the greatest seq means this server history does not
match the client. The client bootstraps again instead of skipping
data.
- A new device must recover synchronized user state from the server.
Unsynchronized events cannot be recovered from a lost device.
- library_id stays fixed for the life of one database file. Noticing
that the file was replaced by a different library, and forcing a new
bootstrap from that change, is Day 2.
- A later optional SSE endpoint may nudge open clients when seq
advances. Nudge only: latest seq or keepalive comments, never action
bodies. Clients still catch up with GET /actions?since=. A missed
nudge is harmless. Do not stream the log over SSE, and do not add a
seq-returning keepalive for clients that already pull on foreground.

### Release and service

- A GitHub release builds the server binary for linux/amd64 and
linux/arm64.
- The install instructions are in README.md. They download the latest
install.sh and run it. A checkout is not required. That script
installs the server. When the binary and configuration are already
present, the same script upgrades: it replaces the binary, the unit,
and uninstall.sh, and appends config keys that are not set yet.
- The systemd unit is enabled at boot and restarts on failure. The
service runs as the music-server user, which needs read access to
the music folder.
- uninstall.sh is installed next to the binary at
/usr/local/bin/uninstall.sh. It removes the service and the binary.
It asks whether to leave the database and artwork cache, or to
remove those too. --keep-data and --purge make that choice without
asking. The conf file and the music folder stay.
- The server reads /etc/music-server/music-server.conf. The shipped
default lists music_root. The installer asks for the music folder
and writes it there. Later settings, including the account allowlist,
are added to this file. The database path, listen address, and scan
interval stay server defaults. The database and the artwork cache
live under /var/lib/music-server.

## Development strategy

Prove architecture risks before adding operations around them.

### Milestone 1: library inventory

Choose one music folder, scan it, and keep a durable inventory.
Programs read that inventory as JSON.

Acceptance:

- Initial import assigns stable asset identities.
- An unchanged rescan creates no duplicate assets.
- Moving or renaming a file inside the root preserves identity when
the bytes are unchanged, and reports neither a new asset nor a deletion.
- Replacing the bytes at a song's path keeps the identity and updates
the hash, when those bytes are new and the previous bytes are gone.
- A move together with a byte change is a new song. The old song is
reported missing and its row is kept.
- Identical copies share identity while remaining at each path.
- Missing files are reported without deleting history.
- An inventory that fails or stops early does not report files as
added or removed.
- Source files are never modified.
- The JSON lists each asset once, where it is now, and which files
are missing.

Use copied sample files, not the production music collection.
No accounts or synchronization in this milestone.

### Milestone 2: synchronization spike

Use one Go server and two lightweight simulated clients with separate
SQLite databases and durable event queues.

Begin with play events and a like/unlike operation. Defer playlists
until the core protocol works. Finish and skip are accepted in
Milestone 3.

Prove:

- Two offline clients of one user converge after reconnecting. A second
server-bound user stays isolated. A user ID in the payload is not
that binding.
- A song added after bootstrap arrives as a later action on the same
cursor as plays and likes.
- Resending an accepted action, or reapplying a received one, does not
apply it twice.
- A lost response after server commit is safe to retry.
- Pending client actions survive restart.
- Server state survives restart.
- Conflicting likes resolve by server accept order.
- Bootstrap reads catalog and analytics at one seq during concurrent
writes, then catches up from the action log.
- Client recovery preserves pending local actions.

### Milestone 3: accounts, finish and skip, and running the server — current task

- Accept play, finish, and skip with the definitions above. A retry
of the same action id does not apply twice.
- Sign-in is Google OAuth 2, and the allowlist file is the only list
of users.
- A GitHub release produces the server binary.
- systemd runs the server on the Debian host.

### Day 2

- A replaced database with a different library_id forces a new
bootstrap.

### Later

- Playlists, only after their conflict rules are written down.
playlistAdds, the count of songs added to a playlist, comes with that.

## Non-goals

- Search or a full-text index on the server. Each client builds its
own index.
- Playback, an offline song cache, and a native iPhone client.
- Replacing or adapting the standalone OpenSubsonic client.
- Spotify Connect-style remote playback.
- Recommendations, YouTube integration, or polished UI.
- Microservices, brokers, distributed databases, or plugin frameworks.
- Public exposure or production data migration.
