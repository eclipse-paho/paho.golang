# Delivery tracking

`delivery.Tracker` enables users to detect when a message has been delivered to the server. It does this by attaching an 
application ID to an outgoing publication and reporting outcomes through `OnResult`. It works with Paho and AutoPaho, 
including the AutoPaho queue (including publications restored from a persistent session).

Example (see the [full example](../../../autopaho/examples/delivery) for more detail):

```go
tracker := delivery.New(delivery.Config{
    OnResult: func(r delivery.Result) {
        log.Printf("publication %s: outcome=%d trigger=%d reason=0x%02x error=%v",
            r.ID, r.Outcome, r.Trigger, r.ReasonCode, r.Err)
    },
    OnError: func(p delivery.Problem) {
        log.Printf("tracking %s (packet %d): %v", p.ID, p.PacketID, p.Err)
    },
})
cfg.ClientConfig.PublishEvents = tracker.Handler() // Set before autopaho.NewConnection.

// After creating the connection manager, register each message before enqueueing.
p := &paho.Publish{QoS: 2, Topic: "example/delivery", Payload: []byte("hello")}
id, err := tracker.Track(p)
if err != nil {
    return err
}
// Record id in your application's pending set here (before a callback can arrive).
log.Printf("queueing %s", id)
if err := cm.PublishViaQueue(ctx, &autopaho.QueuePublish{Publish: p}); err != nil {
    tracker.ReportNotSent(p, err)
    return err
}
```

Outcomes passed to `OnResult` are:

| Outcome | Meaning |
| --- | --- |
| `SendAttempted` | QoS 0 reached `BeforeSend` (prior to network write). Each attempt produces a notification with the same application ID. |
| `Acknowledged` | QoS 1 received successful PUBACK, or QoS 2 received PUBCOMP `0x00`. |
| `Rejected` | The broker rejected the message with PUBACK or PUBREC. `Err` wraps `ErrRejected`; inspect `ReasonCode` and `ReasonString`. |
| `Unknown` | The session was lost (`ErrSessionLost`) or PUBCOMP did not confirm delivery (`ErrUnconfirmed`). |
| `NotSent` | The caller used `ReportNotSent` to report a failure before the message was accepted for delivery. |

`Trigger` contains the MQTT packet type, or zero when no packet caused the event. `ReasonCode` and `ReasonString` 
describe that packet. Results contain no borrowed packet references, so the application may retain them.

The tracker keeps no history of QoS 0 attempts. Applications interested only in the first attempt can remove the ID
from their pending set and ignore subsequent notifications for it. `SendAttempted` does not confirm a successful write
or broker receipt, so it is not a safe signal to shut down the connection immediately.

## Errors and callback ownership

`OnError` reports store/read/cleanup and tracking problems. It does not necessarily mean a publication failed: a 
retained packet may complete later. A cleanup error can produce both an acknowledged result and an error notification. 
If the stored source cannot be read, `Problem.ID` is empty; the tracker cannot safely infer an application ID from the 
MQTT packet ID. Bulk session cleanup errors likewise cannot be assigned to an individual publication.

Session loss reports `Unknown` for each identifiable discarded publication. Messages still in AutoPaho's queue are 
unaffected. Ordinary reconnects retaining the session do not end tracking.

`ReportNotSent` is for known failures before delivery was accepted, such as an enqueue failure. Do not feed every 
`Publish` error into it: timeouts and network errors can occur after the message has entered persistent session state.

Callbacks run synchronously, may overlap, and must return promptly. They must not wait for MQTT operations, disconnect, 
or reconnect. 

The tracker starts no worker goroutines. Assigning `Handler()` replaces existing `PublishEvents`; compose hooks 
explicitly if other extensions also use them, observing the session callback ownership rules.

## Persistence and metadata

`Track` copies properties and their User slice before adding `paho-delivery-id`. The handler retains that property in 
storage, copies it into PUBREL for QoS 2, and removes it from every wire copy without changing the stored packet. 
Publications without the property are ignored.

`Handler()` enables `ReadStoredPacket`, opting into the extra store reads required to recover IDs. Do not disable it on 
the returned handler.

After an application restart, install the handler before connecting and use the same persistent queue/session stores and 
client ID with appropriate session expiry and clean-start settings. The new tracker reads IDs from recovered packets; 
they do not need to be registered again. 

Notifications can be missed or repeated across crashes. Keep durable application records and make completion handling 
idempotent. Results are not deduplicated by this extension, avoiding a permanent in-memory history of completed IDs.

<!-- AI Disclosure: Largely generated with OpenAI Codex (GPT-6).
AI-generated portions are made available under CC0-1.0.
SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
Assisted-by: OpenAI GPT-6 -->
