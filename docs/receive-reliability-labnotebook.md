# OpenMessage receive-reliability lab notebook

Goal: determine why openmessage silently stops receiving Google Messages SMS/RCS
while reporting `connected:true`. Change ONE knob at a time, send a real test
message, record whether/when it arrives.

## Fixed facts (established before the experiment)
- Symptom: daemon `connected:true`, no reconnect, but no new SMS/RCS for ~1h40m
  (10:03→11:43 AM Jul 14). WhatsApp (separate conn) kept flowing. Restart's
  backfill recovered the missed messages.
- Receive path = `ReceiveMessages` long-poll (`libgm.pollReceive`/`readLongPoll`).
- Two stall detectors in libgm:
  1. ditto-ping response-wait → timeout → reconnect. **Disabled in inactive mode
     by our fire-and-forget change (`longpoll.go:223-238`).**
  2. `shouldDoDataReceiveCheck` → extra `GET_UPDATES`. Interval =
     `DefaultBugleDefaultCheckInterval = 2h55m` (`longpoll.go:246`). Very slow.
- Knobs available WITHOUT rebuild (launchd env):
  - `OPENMESSAGE_INACTIVE` (0/1) → ditto `isActive` true/false
  - `OPENMESSAGE_PASSIVE` (0/1) → `DontMarkActive` (skip presence ping entirely)
- Knobs needing rebuild: fire-and-forget logic, data-receive-check interval,
  openmessage-side periodic reconcile.

## Hypotheses
- H1: `isActive=false` makes Google stop delivering to the long-poll (design).
  Counter-evidence: real web client receives while backgrounded.
- H2: fire-and-forget disabled fast stall detection; long-poll stalls and isn't
  recovered for ~3h (bug we introduced).
- H3: something else (cookie/session, competing web session, etc.).

## Test protocol
1. Note config + connection age (time since last "Connected to Google Messages").
2. Send a text from voice.google.com to the paired cell number.
3. Wait ~90s. Query messages.db for the new inbound message.
4. Record: received? latency? daemon log events during window.

## Runs
(see table below; newest last)

| # | time | config | conn age | test msg | received? | latency | notes |
|---|------|--------|----------|----------|-----------|---------|-------|
| A | 13:35 | INACTIVE=1 PASSIVE=0 (deployed, no rebuild) | ~1h44m (conn since 11:51) | LABTEST-A-1334 (GV→cell) | NO (>6min) | — | see notes A |

### STALL CONFIRMED (Run A)
- 13:42→13:54 watch: newest SMS frozen at 13:30:51 the whole time; LABTEST-A-1334
  (sent 13:35) never arrived. 24 min of total receive silence, `connected:true`,
  no reconnect. Stall onset ~13:30, ~1h39m after the 11:51 (re)connect.
- Live log around today's earlier reconnects: ping SEND failures (DNS i/o timeout,
  401) ARE detected → reconnect. A long-poll that goes silent while pings still
  SUCCEED is NOT detected → the silent stall. Root mechanism: our fire-and-forget
  removed the ping-response-wait, which in active mode confirms the long-poll is
  alive (ping ack returns via the long-poll). Fallback GET_UPDATES check = every
  ~3h. So a silently-dead long-poll goes unnoticed for hours.
- KEY UNKNOWN → deployed HEALTHPROBE build (gmessages 1c8639f, openmessage 7afdc5d):
  logs whether each isActive=false ping is ACKED via long-poll or TIMES OUT. If it
  TIMES OUT exactly when receive stalls → response-wait is a valid dead-long-poll
  detector → fix = reconnect on timeout. If ACKED during a stall → need a different
  detector (openmessage-side periodic reconcile).

| B | 14:05 | INACTIVE=1 + HEALTHPROBE build | fresh (conn 14:04) | LABTEST-B-1405 | YES @14:06:01 (~1min) | — | fresh conn receives fine |

### KEY FINDING (Run B) — the crux
- HEALTHPROBE: isActive=false ditto pings **TIMED OUT** at 2:05 and 2:06, *while*
  test B was being delivered through the long-poll at 14:06:01. So an inactive
  ping's ack NEVER comes, whether the long-poll is alive or dead.
- => the ping response-wait CANNOT detect a dead long-poll in inactive mode
  (always times out). "Reconnect on ping timeout" is a dead end (false positives).
- => FIX must be independent of long-poll liveness: an openmessage-side periodic
  reconcile via the request/response API (ListConversations/FetchMessages), which
  stays valid during a stall (pings still SEND ok => session valid). Implemented as
  OPENMESSAGE_RECONCILE_SECS (default 120s) periodic reconcile.

| C | 14:14 | INACTIVE=1 + reconcile-fix (RECONCILE_SECS=120) | fresh (conn 14:12) | LABTEST-C-1414 | YES | fast | received on fresh conn (long-poll healthy) |

### Run C notes — fix mechanism CONFIRMED firing
- Periodic reconcile fires reliably every 2 min: log shows "Reconciling recent
  conversations conversation_limit=12 ... reason=periodic" at 2:18,2:20,2:22,2:24,
  2:26,2:28PM. So the safety-net pull runs as designed, independent of long-poll.
- HEALTHPROBE keeps timing out every ping (expected; inactive pings never acked).
- No reconnects during the window; receive fresh.
- REMAINING PROOF: catch a natural long-poll stall (>~1.5h uptime) and confirm a
  test RCS still arrives within ~2min via the reconcile while the long-poll is
  dead. Prior strong evidence it will: the 11:43 restart's ListConversations
  backfill recovered the 10:03→11:43 messages, i.e. the same API works while the
  session is valid (pings send fine during a stall). Continuing the soak to the
  stall window (~15:42) to get the empirical confirmation.
- Note: background soak watchers keep getting culled (~13min); switching to
  wakeup-paced point checks.

| D | 15:11 | INACTIVE=1 + reconcile-fix | ~58m (conn 14:12) | LABTEST-D-1510 | YES @~15:11:20 (~10s) | fast | long-poll healthy at 58m — no natural stall. |
| E | 15:54 | INACTIVE=1 + reconcile-fix | ~1h40m (conn 14:12) | LABTEST-E-1553 | YES @15:54:02 (~10s) | fast | long-poll STILL healthy at 1h40m (prior stall mark) — stalls are intermittent, not deterministic by uptime. Switched to a FORCED test. |
| F | 15:58 | INACTIVE=1 + reconcile-fix + **OPENMESSAGE_DROP_LONGPOLL=1** | fresh (conn 15:57) | LABTEST-F-1558 | **YES @16:00:00 (~80s)** | 80s | **DECISIVE. Long-poll deliveries forcibly dropped (2× "DROP_LONGPOLL: ignoring real-time message event" logged) → simulated a silently-dead long-poll. F still arrived via the 3:59PM periodic reconcile (~80s). Fast (~10s) for healthy long-poll vs 80s on a reconcile tick when dropped = exactly the expected signatures.** |

## CONCLUSION — fix confirmed
Root cause: the modern Google Messages long-poll silently goes deaf (delivers
nothing) while still `connected`. In inactive-presence mode (isActive=false, kept
for phone notifications) the ditto ping is never acked, so libgm cannot detect a
dead long-poll; its fallback check is ~3-hourly. Result: inbound SMS/RCS silently
stops for hours.

Fix: openmessage-side periodic reconcile via the request/response API
(ListConversations/FetchMessages) every OPENMESSAGE_RECONCILE_SECS=120s. Proven
(run F) to deliver messages within ~80s even with the long-poll's real-time
delivery fully disabled. Keeps isActive=false → phone notifications AND reliable
receive. Under normal operation the healthy long-poll still delivers in ~10s; the
reconcile is the safety net that bounds worst-case receive latency to the interval.

Cleanup pending: remove the noisy HEALTHPROBE WRN logging (gmessages); keep the
DROP_LONGPOLL debug flag (default off) and the periodic reconcile.

## PHASE 2 — do it properly, drop the reconcile hack (user directive)
The reconcile is a request/response poll bolted on top of a streaming API — a
workaround, not what the real client does. Capture (CAPTURED_FINDINGS §2.8–2.9):
the real messages.google.com/web client does NOT poll; it holds a streaming
`ReceiveMessages` long-poll whose response interleaves data frames and server
**heartbeat** frames, and a parallel `PullMessages` heartbeat long-poll. It stays
healthy because it DETECTS a dead stream (via the heartbeat pulse / acked
keepalives) and reconnects.

libgm's gap (longpoll.go readLongPoll): in foreground mode there is **NO read
deadline** — `reader.Read` blocks forever, so a silently-dead stream (no data, no
heartbeat) is never noticed. That's the true root cause; the isActive=false mode
just also disables the ditto-ping detector. Proper fix = idle read-deadline:
if no frame (data OR heartbeat) arrives within the heartbeat interval + margin,
close and reconnect — exactly what the real client effectively does.

Pivotal unknown: does the server heartbeat an isActive=false stream regularly
(=> clean deadline) or go quiet when "inactive" (=> deadline would thrash)?
Deployed STREAMPULSE instrumentation (gmessages 7957cd8, openmessage d4b9d38) to
log every ReceiveMessages frame's inter-arrival gap.

### HEARTBEAT CADENCE MEASURED → deadline is viable
Steady-state heartbeat gap = **10.00s, very tight** (9993–10085ms), on the
isActive=false stream. So the server heartbeats us regularly regardless of
"inactive" presence → a read-deadline of 30s (3 missed heartbeats) cleanly
detects a dead stream with no false-positive risk.

### FIX IMPLEMENTED (gmessages dd570f8, openmessage 091cffab)
longpoll.go readLongPoll: foreground idle read-deadline. Any frame (data or
heartbeat) rearms a timer; if it fires (no frame within ReceiveIdleTimeout,
default 30s), close THIS poll's connection (rc.Close(), NOT closeLongPolling
which bumps listenID and would end the loop) so pollReceive reopens it →
reconnect. Timeout tunable via OPENMESSAGE_RECEIVE_IDLE_SECS.

### VALIDATION (in progress) — reconcile OFF (RECONCILE_SECS=0)
- Aggressive test: RECEIVE_IDLE_SECS=5 (< 10s heartbeat) forces the idle-fire +
  reconnect path every ~5s. Confirm: idle-fires logged, and probes still arrive
  despite constant reconnects (proves the reconnect path delivers, no message
  loss across reconnects).
- Then RECEIVE_IDLE_SECS=30 (default): confirm ZERO idle-fires under healthy
  operation (no thrash) and probes arrive fast via the healthy long-poll.
- Then REMOVE the periodic reconcile + all diagnostics (STREAMPULSE), keep the
  idle-deadline as the sole fix.

### VALIDATION RESULTS
| test | config | result |
|---|---|---|
| aggressive | RECONCILE=0, IDLE=5s | **PASS.** 12 idle-fires in 60s (~1/5s), each reconnects. Probe H (LABTEST-H-1638) sent ~16:38 **arrived 16:39:50 despite constant ~5s reconnect churn** with reconcile OFF. => reconnect path delivers, no loss across reconnects. Google re-delivers queued msgs on reconnect. |
| no-thrash | RECONCILE=0, IDLE=30s (default) | in progress — expect ZERO idle-fires (heartbeat 10s < 30s) + fast probe delivery. |

Note: a natural/forced 30s-silence stall triggers the SAME reconnect path already
proven in the aggressive test, so recovery is validated; the 30s soak only needs
to confirm no false-positive fires under healthy heartbeats.

## PHASE 2 CONCLUSION — proper fix shipped, reconcile removed
- No-thrash soak (idle=30s, 15 min): idle_fires delta = **0**; heartbeats steady
  at ~10s. No false positives.
- Finalized: removed the periodic reconcile, STREAMPULSE, DROP_LONGPOLL, HEALTHPROBE.
  Sole receive-reliability fix = the libgm idle read-deadline (gmessages b632fa6,
  openmessage 131608, home.nix e5daa0f). Env now: INACTIVE=1 + dedicated cookie
  profile only; RECEIVE_IDLE_SECS unset (30s default).
- Final clean build: 0 STREAMPULSE, 0 periodic reconcile, 0 idle-fires; probe I
  (LABTEST-I-1704) arrived ~10s via the healthy long-poll.

### Root cause (final, one line)
libgm's foreground ReceiveMessages read loop had no read deadline, so a
silently-dead stream (no data/heartbeat/error) blocked reader.Read forever and
inbound messages stopped. Fix = idle read-deadline (30s = 3 missed ~10s
heartbeats) → close+reconnect, matching the real web client's liveness detection.
Not a Google bug; not a poll hack.

### Run A notes
- Test method VALIDATED: prior GV self-texts (+14152301367 → cell) are in the DB
  (e.g. "testing message to myself" 07-11 01:53), so GV→cell→openmessage normally
  syncs. 17 msgs historically from that GV number.
- Daemon was NOT stalled by age: it received real inbound at 12:36, 12:54
  (+19257856488 "Correct" — the previously-broken thread), and **13:30:51** — i.e.
  fine at ~1h40m uptime. So "aged connection stalls" (H2 by age) is NOT supported.
- BUT: newest received = 13:30:51; I sent LABTEST-A-1334 at 13:35 → not received by
  13:41. Nothing received in the 13:30→13:41 window. Two live possibilities:
  (a) a stall began ~13:30 (my test hit it), or (b) GV self-msg latency. Watching.
- REVISED PICTURE: the real 10:03→11:43 stall happened after the session had been
  connected since ~01:55 (≈8h uptime), ended only by my manual restart. Points at a
  LONG-timescale trigger (token/session/cookie aging or a discrete event), NOT a
  ~1.5h age. Need longer observation.

## PHASE 3 — CORRECTION: the reconcile is REQUIRED (removing it was wrong)
Empirical proof (2026-07-14 ~23:44): with idle-deadline only (reconcile removed),
openmessage received NOTHING for 2.5h while the long-poll was fully healthy —
reopening every ~15min (HTTP 200), heartbeats flowing, ditto pings succeeding,
0 idle-fires, 0 reconnects. "Rose's reply" (707-561-2671, 22:27) and ~2.5h of
other messages were WITHHELD from the live stream and recovered instantly by one
ListConversations pull (restart's listen_recovered reconcile).

Conclusion: there are TWO stall modes, needing TWO mechanisms:
1. DEAD stream (no data/heartbeat) → libgm idle read-deadline reconnects it.
2. LIVE-but-WITHHOLDING stream → Google does not stream inbound to an
   isActive=false (inactive) client; it routes to the phone. The ONLY fix is a
   periodic request/response pull (reconcile). Not a hack — it is the receive
   path for an inactive client. The real web client never needs it because it is
   either active (streamed) or closed (not receiving); openmessage uniquely wants
   inactive+receiving (phone notifications + archive), which Google's streaming
   does not serve.

Restored: reconcile (openmessage 2294b4d, home.nix d18bd69, RECONCILE_SECS=120)
alongside the idle-deadline (gmessages b632fa6). Both now deployed.

Still open: the "Device pairing" phone notification appears intrinsic to the
isActive=false presence signal (the same signal that makes the phone notify) —
a Google-design tradeoff, not yet resolved.

## PHASE 4 — 2026-07-15: replicate the web client exactly (isActive=true, no reconcile)
User directive: stop approximating; observe a real backgrounded messages.google.com
tab and make openmessage behave identically.

### Run J — backgrounded real web tab (default Chrome profile, separate GAIA pairing)
- Opened messages.google.com (already GAIA-paired, no QR), captured startup RPCs:
  SignInGaia ×2, ReceiveMessages (both main + -jms-us hosts), PullMessages,
  ListIdentities, SendMessage-wrapped ditto, AckMessages. ZERO ListConversations.
  (Also kills the "old API" hypothesis from notification-suppression-investigation.md:
  the real web client uses ReceiveMessages too.)
- Backgrounded the tab (focused a Voice tab), cleared its network log, sent
  LABTEST-J-1300 via voice.google.com → the backgrounded tab received it LIVE:
  only new traffic = 1 AckMessages + 1 ditto SendMessage. **A backgrounded tab
  keeps streaming; backgrounding ≠ inactive.**
- Daemon (INACTIVE=1, RECONCILE=120) also got J, but timing was ambiguous vs the
  reconcile tick (ticks at :52 of odd minutes; J landed at 16:03:15, tick 16:03:52).

### Run K — tick-timing disambiguation (daemon still INACTIVE=1)
- Sent LABTEST-K-1608 at 16:08:37, just AFTER the 16:07:52 tick.
- 16:09:03 (26s): NOT in DB. Appeared exactly at the 16:09:52 tick.
- **The inactive daemon's stream did not deliver; the reconcile did.** (Probe B
  precedent shows live stream delivery is ~seconds when it happens at all.)

### Run L — does an ACTIVE client suppress phone notifications? (the false premise)
- LABTEST-L-1615 sent with the active web tab open: **phone notified normally.**
- So active-client-suppresses-phone is FALSE for a real web session. The earlier
  "INACTIVE=1 CONFIRMED restores phone notifications" finding predates the
  dedicated-profile fix, when reconnect churn re-ran SetActiveSession every ~18min.

### Run M — daemon flipped to INACTIVE=0, RECONCILE_SECS=0 (home.nix, env only)
- LABTEST-M-1624 sent 16:24:41 → in DB by 16:24:57, reconcile disabled.
  **Active daemon streams like the tab.**

### Runs N/O — the NEW withholding mode: reopen without re-assertion
- 16:42 WRN "Stopped reading data from server: connection reset by peer".
  Poll reopened silently (2 ESTABLISHED conns to the receive host, status
  connected, heartbeats fine).
- LABTEST-N-1644 and LABTEST-O-1647: NEVER delivered on the reopened stream
  (>4 min), recovered only by the next restart's backfill.
- **Google stops fanning out to a session whose stream reconnected without a
  fresh activity assertion.** This also reframes Phase 3: the 2.5h withholding
  followed reopens with no re-assertion (inactive client never re-asserts).
  The web client re-asserts on every tab hidden→visible transition, so its
  reopens are always re-blessed shortly after.

### Fix (gmessages a1df7d9, openmessage e9c4fae): reassertActiveSession
- After every long-poll reopen except the first (postConnect covers that), send
  GET_UPDATES on the EXISTING session (same call as HandleNoRecentUpdates).
  Unlike SetActiveSession there is no ResetSessionID, so no "Device pairing"
  phone notification.
- Run P — fresh connect after deploy: LABTEST-P-1700 sent 17:00:01, in DB by
  17:00:32 via stream (reconcile still 0).
- Run Q — PASS: stream reopened 17:11 ("Long polling opened" + "Re-asserted
  active session after long-poll reopen" in debug log); LABTEST-Q-1713 sent
  17:12:35 → in DB by 17:13:01 via the reopened stream. The exact reopen mode
  that swallowed runs N/O now delivers. Bonus: with isActive=true the ditto
  pings are acked again (~300ms every minute), restoring libgm's ping-based
  liveness detection that inactive mode had lost.

Config end state: INACTIVE=0, RECONCILE_SECS=0, PASSIVE=0, dedicated Chrome
profile, idle read-deadline 30s. If run Q fails or phone notifications regress,
revert env to INACTIVE=1 + RECONCILE_SECS=120 (both paths still in the code).

## PHASE 5 — 2026-07-15/16 night: isActive is the ring-suppressor; drop the pings
- User report (~23:38): with INACTIVE=0 (isActive=true pings every minute) the
  phone stopped vibrating for inbound. So the minute-cadence isActive=true ping
  continuously re-suppresses phone rings — run L was not a contradiction: the
  TAB doesn't ping while backgrounded.
- Run R RETRACTED: the probe was never actually sent (browser click missed the
  reply box after the tab idled; the Voice thread shows no LABTEST-R). The
  "INACTIVE=1 + reassert" configuration therefore remains UNTESTED; the only
  clean evidence for inactive-mode fan-out revocation is still run K.
- New mode (gmessages 29eb9e2 SkipDittoPings; openmessage a45e44b
  OPENMESSAGE_NO_PINGS=1): backgrounded-tab replica — SetActiveSession once on
  connect, NO periodic NOTIFY_DITTO_ACTIVITY at all, silent GET_UPDATES
  reassert on every reopen; liveness = 30s idle read-deadline.
- Run S — PASS on BOTH axes: LABTEST-S-2352 sent 23:51:03 → in daemon DB by
  23:51:31 via stream (no reconcile), AND the phone vibrated (user-confirmed),
  ~90s after the connect-time SetActiveSession. Stream delivery + ringing
  phone simultaneously, for the first time.
- Run T — PASS: stream reopened 00:02 (+ "Re-asserted active session" in debug
  log); LABTEST-T-0008 sent 00:07:28 → in DB by 00:07:56 via the reopened
  stream. No-pings mode survives reopens. VALIDATION COMPLETE — deployed
  config: NO_PINGS=1, INACTIVE=0, RECONCILE_SECS=0.

## PROBE INDEX (quick reference, all runs)
| Probe | When (PT) | Daemon config at send | Result |
|-------|-----------|----------------------|--------|
| A-1334 | 07-14 13:35 | INACTIVE=1, pre-fix | not received → revealed the stall |
| B-1405 | 07-14 14:05 | fresh conn | received via stream (~s) |
| C-1414 | 07-14 14:13 | reconcile-fix build | received via reconcile |
| D-1510 | 07-14 15:11 | stall window | diagnostics |
| E-1553 | 07-14 15:53 | stall-mark build | diagnostics |
| F-1558 | 07-14 15:58 | DROP_LONGPOLL, reconcile-only | received via reconcile |
| G-1601 | 07-14 16:01 | normal restore | received |
| H-1638 | 07-14 16:39 | idle=5s churn test | received through forced reconnects |
| I-1704 | 07-14 17:05 | final clean build (idle-deadline+reconcile) | received ~10s |
| J-1300 | 07-15 16:03 | INACTIVE=1 RECONCILE=120; bg TAB open | tab: streamed+acked; daemon: path ambiguous |
| K-1608 | 07-15 16:08 | same | daemon stream did NOT deliver; reconcile tick 16:09:52 did |
| L-1615 | 07-15 16:18 | same; bg tab open | PHONE VIBRATED with active tab present |
| M-1624 | 07-15 16:24 | INACTIVE=0 RECONCILE=0, fresh connect | streamed ≤11s |
| N-1644 | 07-15 16:43 | same, after 16:42 conn reset | NEVER delivered on reopened stream |
| O-1647 | 07-15 16:46 | same | NEVER delivered; recovered by restart backfill |
| P-1700 | 07-15 17:00 | + reassert-on-reopen (a1df7d9), fresh connect | streamed ≤30s |
| Q-1713 | 07-15 17:12 | same, 1 min after 17:11 reopen+reassert | streamed ≤26s — reassert validated |
| R-2341 | 07-15 23:41 | INACTIVE=1 RECONCILE=0 | RETRACTED — probe never actually sent (misclick) |
| S-2352 | 07-15 23:51 | NO_PINGS=1 (SkipDittoPings), fresh connect | streamed ~5s AND phone vibrated — dual PASS |
| T-0008 | 07-16 00:07 | NO_PINGS=1, 5 min after 00:02 reopen+reassert | streamed ≤28s — no-pings mode survives reopens. FINAL |

Config timeline 07-15: INACTIVE=1+RECONCILE=120 until 16:22 → INACTIVE=0+RECONCILE=0
16:22–23:40 (phone silent per user; N/O lost pre-reassert; reassert deployed 16:59) →
INACTIVE=1+RECONCILE=0 23:40–23:49 (untested, R retracted; receive path broken) →
NO_PINGS=1 23:49– (run S dual pass).
