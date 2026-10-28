# Pre-launch schema and protocol review

Written before production. The system is not deployed yet, so schema and
protocol can still change in place. After launch, the action log and
derived tables become hard to revise.

Scope: Opus (`music-server`), Hush (`music-client`), `music-web`. The live schema is
`internal/inventory/store.go`. Product rules are REQUIREMENTS.md.
`database.md` was deleted; it had drifted from the code.

## Verdict

The core model is sound: one song id, one append-only log, bootstrap then
catch up, likes as an ordered list, analytics as counters. Freeze the
action set below for launch. Reset schema versioning before first real
install. Playlists stay deferred until conflict rules exist.

---

## Frozen action types (launch)

Keep this set small. Payloads are empty objects or one clear field.
Spotify’s public model is the same idea: discrete listen events, a
saved/liked list with order, and aggregates derived later — not one
blob that tries to be everything.

| type | payload | fold | Spotify analogue |
| --- | --- | --- | --- |
| `song_upsert` | `{hash, available, metadata, artwork?}` | replace catalog fields | catalog update |
| `song_unavailable` | `{"available": false}` | set unavailable | delisted / missing |
| `play` | `{"listenedMs": N}` `N > 30000` | +play_count, +listened_ms, last_played | stream (~30s) |
| `finish` | `{}` | +finished_count | trackdone / natural end |
| `skip` | `{}` | +skip_count | forward skip |
| `like` | `{}` | insert liked if absent | save / heart |
| `unlike` | `{}` | delete liked row | unsave |

Why this freezes well:

- Listening events stay independent (play does not imply finish or skip).
- Membership is only like/unlike with `{}`. Playlist ops must not reuse them.
- Catalog payloads carry the device view of one song; paths stay off the wire.
- New product work adds new types after REQUIREMENTS conflict rules, e.g.
  `playlist_add` / `playlist_remove`. Do not widen `like`.

Do not add before launch: `save`, `playlist_add`, scrub events, or a
combined “listen session” action. Those either duplicate the set above
or lock a product shape you have not defined.

---

## Analytics table — keep as counters

Current columns (per user, per song):

- `play_count`, `finished_count`, `skip_count`
- `listened_ms` (sum of play payloads)
- `last_played_at_ms` (descriptive; from play `occurredAtMs`)

Opinion: this is the right shape for launch.

Spotify separates three things we already separate:

1. **Event stream** — our `actions` log (stronger than Spotify’s public
   recently-played window of ~50 plays).
2. **Saved / liked list** — our `liked` table with order (like Liked Songs
   / saved tracks with date added).
3. **Aggregates** — Wrapped-style totals. Our `analytics` row is that
   materialization so bootstrap does not force every client to replay
   every play.

What not to put in analytics:

- Liked (already moved out).
- Recently played as a list (query `play` actions by seq when that screen
  exists; do not invent a second playlist-shaped table).
- Playlist membership or add counts (wait for playlist rules).

`finished_count` and `skip_count` are worth keeping: they match how
Spotify and Apple treat natural end vs forward skip as distinct from a
stream, and they stay cheap counters.

`last_played_at_ms` is enough for “sort my library by last play.” It is
not a history. History is the log.

---

## Hardest contracts after launch

1. **Action log** — rows are never rewritten; fold every historical payload.
2. **Song identity** — UUID `song_id`; `content_id` / wire `hash` is bytes only.
3. **Bootstrap** — full catalog + per-song analytics + top-level `liked`.
4. **One shared `seq`** — clients must tolerate gaps when other users’ rows are filtered.

---

## Server schema (as implemented)

| table | role |
| --- | --- |
| `meta` | root path, `schema_version` |
| `assets` | song: id, content_id, size, metadata, artwork_* |
| `locations` | server-only paths; never on the wire |
| `library_state` | singleton `library_id` |
| `actions` | append-only log |
| `analytics` | per-user counters |
| `liked` | per-user ordered likes (`id` AUTOINCREMENT) |

Availability is computed from `locations.present`, not stored on `assets`.

`liked`: re-like of an already liked song is a no-op (keeps place). Unlike
then like → new id → top of the newest-first list. If playlists arrive
within a year, decide whether `liked` stays dedicated or becomes playlist
`"liked"` before building playlists.

`schema_version` is still `'1'` with no migration story. Before first real
install: reset to a launch number, or keep wipe-only and document it.

---

## Clients

**iPhone** — `liked` table, overlay pending likes above the list, wipe-only
schema (log out rebuilds). `user_version` is 4; reset to 1 at launch or
document 4 as the launch schema.

**Web** — no SQLite. Snapshot + pending queue in memory / localStorage.
Liked order comes from `snapshot.liked` and applied like/unlike. Screen
updates after the server accepts a like (web rule), not on enqueue.

Sample phone `snapshot.json` may omit `liked` (preview). Live bootstrap
always includes the array.

---

## Remaining pre-launch checklist

1. Reset server and client schema versions to a single launch number.
2. Decide playlist direction enough to know whether `liked` stays forever.
3. Implement Day-2 `library_id` mismatch handling before production restore.
4. Spot-check metadata quality on a full library scan.
5. Keep the frozen action table above; any addition updates REQUIREMENTS first.
