# Delivery tracking

Whilst it may seem straightforward to track message delivery, there are several nuances to consider. For example:

* The client delivers messages to the broker and has no visibility of delivery to subscribers.
* If the application is restarted, messages "sent" from the initial instance may be delivered by the second. 
* The "session" may be lost, meaning partially delivered messages will never be completed.
* autopaho allows messages to be queued which may further delay delivery.

This library supports the use of an MQTT v5 User Property to track message delivery to the broker (the property does not
have to be sent over the wire). It is important to note that this does not mean the message has reached a subscriber.

The [delivery extension](../../../paho/extensions/delivery) uses this base functionality to demonstrate this and provide 
a usable tracker.

This example queues 1,000 QoS 2 messages and exits successfully only after the library has notified it that every message 
has been delivered (PUBCOMP acknowledgement from the broker). It needs an MQTT v5 broker with QoS 2 support; no 
subscriber is required (but feel free to use one).

Note that the example stores the queue and state in memory. This is not recommended for production use (use file-backed  
stores).

To run it from the repository root use:

```sh
go run ./autopaho/examples/delivery -broker mqtt://localhost:1883 -topic example/delivery -count 1000 -timeout 2m
```

The above shows the default options, running without any options will work fine with a local broker. 

The repository's Docker example can provide a local broker: run `docker compose up mosquitto` from 
`autopaho/examples/docker` in another terminal.

The app logs when the batch has been queued and when all deliveries are confirmed. Ctrl-C, timeout, broker rejection, 
session loss, or a tracking error ends the run with a nonzero exit status.

## How tracking works

1. Create a `delivery.Tracker` with `OnResult` and `OnError` callbacks and assign its `Handler()` to `PublishEvents`.
   The handler enables `ReadStoredPacket` to recover IDs; this opts into extra store I/O.
2. The example's `batchTracker.add(message)` calls the extension's `Track(message)` to generate and attach an ID in
   the `paho-delivery-id` User Property. It records that ID in the batch before calling `PublishViaQueue`.
   A queueing error is reported through `ReportNotSent` and aborts the batch.
3. The extension removes the ID from each wire copy and copies it from PUBLISH into PUBREL for QoS 2.
   The example does not implement these packet callbacks itself.
4. `OnResult` removes acknowledged IDs from the batch's pending set. Rejection or unknown delivery fails the batch.
   `OnError` also aborts this example, although a general application may continue waiting after a tracking problem.
5. After queueing the last message, call `batchTracker.doneAdding()`. Success is signalled only when registration has
   ended and no IDs remain pending, so acknowledgements between additions cannot finish the batch early.
   Callbacks never wait for MQTT operations or initiate shutdown.

The extension also supports QoS 1 (PUBACK) and QoS 0 (each transmission attempt).

This example uses QoS 2 to demonstrate tracking.

The example accepts only PUBCOMP with reason code zero; it conservatively reports PUBCOMP `0x92` as unconfirmed delivery, 
although that response can occur during recovery and does not prove the broker failed to deliver the message.

## Reconnects and application restarts

The example uses an in-memory queue, session store, and pending-ID set. It creates a new client ID and batch on each run, 
clears any initial broker session, and requests a 600-second session expiry to allow reconnects during that run. It does
not resume a batch after the process exits.

For application-restart recovery, use persistent queue and session stores, a stable client ID with 
`CleanStartOnInitialConnection: false`, and a durable record of pending application IDs. Reinstall the same callbacks 
on startup. Notifications can be missed or repeated across crashes, so application completion handling must be 
idempotent and account for that recovery gap.
