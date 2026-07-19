# Google Messages: phone-notification suppression while the bridge runs

## Symptom

While OpenMessage's Google Messages bridge is connected, the **phone stops
showing notifications** for incoming SMS/RCS. Messages still arrive (in the
bridge and in the phone's Messages app), but the phone no longer buzzes/banners.

This got worse after the reconnect self-heal fix (`fix/google-cookie-selfheal`):
before it, the session died every ~18 min and was disconnected most of the time,
so the phone notified during the gaps. Once the session stayed up 24/7, the phone
went permanently silent.

## What we ruled out (all confirmed, not guessed)

The `libgm` client behaves like an **always-foreground** Messages-for-web tab.
We compared it against the real web client (`messages.google.com/web`) by
observing its RPCs via `PerformanceObserver` resource timing while toggling
`document.visibilityState`, and by an isolation test.

1. **`SetActiveSession()` on connect** — the real client sends its "active"
   assertion (a `SendMessage`) only on the `hidden→visible` transition and
   nothing when backgrounded. Added `libgm.Client.DontMarkActive` to skip it
   (opt-in via `OPENMESSAGE_PASSIVE=1`). Receiving still worked → **not the
   cause on its own; phone still silent.**
2. **Browser-presence acks** — under `DontMarkActive` we also stop acking
   `BrowserPresenceCheckEvent`. Still silent.
3. **The periodic `NotifyDittoActivity` ping** — this is `libgm`'s connection
   heartbeat (send ping, timeout ⇒ reconnect) but doubles as an "active client"
   assertion. Under `DontMarkActive` we skip it too; the long-poll self-maintains
   and still receives. **Still silent.** (Downside: removes `libgm`'s active
   liveness detection, leaning on the long-poll erroring + the reconnect watchdog
   + the ~3h data-receive check.)
4. **Marking messages read** — OpenMessage does **not** call `libgm.MarkRead` on
   receipt, and the real backgrounded client acks + keeps the message **unread**
   (`(1)` badge) yet the phone still notifies. So acking/read-state is not it.

### Isolation test (decisive)

- OpenMessage **off** + real web client open & **backgrounded** → **phone
  notifies.** So a backgrounded web client does *not* suppress.
- OpenMessage **on** (even fully passive) → **phone silent.**

⇒ The suppressor is **OpenMessage/`libgm`-specific**, and per the "delivery fans
out to phone + all devices in parallel" model, it's a **connect/registration-time
property**, not a receive-time action. OpenMessage reuses its paired session, so
it sends no per-connect device-register call; the difference is structural.

## The remaining (structural) suspect: API generation

The real web client talks the **current** API — `PullMessages` /
`SendMessage` / `AckMessages` / `SignInGaia`. `libgm` (this fork, frozen at an
older point) talks the **older** API — `ReceiveMessages` + `GET_UPDATES`
(`util/paths.go: ReceiveMessagesURL`, `gmproto` has only `ReceiveMessagesRequest`).
A device registered on the older protocol appears to be treated by Google's
notification router as the notification owner, suppressing the phone.

This is the single most promising untried lever, but it is **not a flag**: the
new RPCs, their protobuf schemas, and the surrounding auth/crypto are not present
in `libgm`. Realistically it requires either upgrading onto a newer upstream
`mautrix-gmessages` that already speaks the new API, or reverse-engineering the
schemas — a large effort, and the hypothesis is still unverified.

## Status of the passive-mode code

`OPENMESSAGE_PASSIVE` / `libgm.Client.DontMarkActive` is a **documented dead end**
for the notification goal: it does not restore phone notifications and it weakens
liveness. It is left in place (default off) as an investigated, reversible
experiment and as scaffolding for future work. Do **not** enable it expecting
notifications to return.
