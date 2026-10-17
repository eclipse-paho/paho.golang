AutoPaho
========

AutoPaho has a number of aims:

* Provide an easy-to-use MQTT v5 client that provides commonly requested functionality (e.g. connection, automatic reconnection, message queueing).
* Demonstrate the use of `paho.golang/paho`.
* Enable us to smoke test `paho.golang/paho` features (ensuring they are they usable in a real world situation)

## Basic Usage

The following code demonstrates basic usage; the full code is available under `examples/basics`:

```go
func main() {
	// App will run until cancelled by user (e.g. ctrl-c)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// We will connect to the Eclipse test server (note that you may see messages that other users publish)
	u, err := url.Parse("mqtt://mqtt.eclipseprojects.io:1883")
	if err != nil {
		panic(err)
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{u},
		KeepAlive:  20, // Keepalive message should be sent every 20 seconds
		// CleanStartOnInitialConnection defaults to false. Setting this to true will clear the session on the first connection.
		CleanStartOnInitialConnection: false,
		// SessionExpiryInterval - Seconds that a session will survive after disconnection.
		// It is important to set this because otherwise, any queued messages will be lost if the connection drops and
		// the server will not queue messages while it is down. The specific setting will depend upon your needs
		// (60 = 1 minute, 3600 = 1 hour, 86400 = one day, 0xFFFFFFFE = 136 years, 0xFFFFFFFF = don't expire)
		SessionExpiryInterval: 60,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			fmt.Println("mqtt connection up")
			// Subscribing in the OnConnectionUp callback is recommended (ensures the subscription is reestablished if
			// the connection drops)
			if _, err := cm.Subscribe(context.Background(), &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{Topic: topic, QoS: 1},
				},
			}); err != nil {
				fmt.Printf("failed to subscribe (%s). This is likely to mean no messages will be received.", err)
			}
			fmt.Println("mqtt subscription made")
		},
		OnConnectError: func(err error) { fmt.Printf("error whilst attempting connection: %s\n", err) },
		// eclipse/paho.golang/paho provides base mqtt functionality, the below config will be passed in for each connection
		ClientConfig: paho.ClientConfig{
			// If you are using QOS 1/2, then it's important to specify a client id (which must be unique)
			ClientID: clientID,
			// OnPublishReceived is a slice of functions that will be called when a message is received.
			// You can write the function(s) yourself or use the supplied Router
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					fmt.Printf("received message on topic %s; body: %s (retain: %t)\n", pr.Packet.Topic, pr.Packet.Payload, pr.Packet.Retain)
					return true, nil
				}},
			OnClientError: func(err error) { fmt.Printf("client error: %s\n", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					fmt.Printf("server requested disconnect: %s\n", d.Properties.ReasonString)
				} else {
					fmt.Printf("server requested disconnect; reason code: %d\n", d.ReasonCode)
				}
			},
		},
	}

	c, err := autopaho.NewConnection(ctx, cliCfg) // starts process; will reconnect until context cancelled
	if err != nil {
		panic(err)
	}
	// Wait for the connection to come up
	if err = c.AwaitConnection(ctx); err != nil {
		panic(err)
	}

	ticker := time.NewTicker(time.Second)
	msgCount := 0
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			msgCount++
			// Publish a test message (use PublishViaQueue if you don't want to wait for a response)
			if _, err = c.Publish(ctx, &paho.Publish{
				QoS:     1,
				Topic:   topic,
				Payload: []byte("TestMessage: " + strconv.Itoa(msgCount)),
			}); err != nil {
				if ctx.Err() == nil {
					panic(err) // Publish will exit when context cancelled or if something went wrong
				}
			}
			continue
		case <-ctx.Done():
		}
		break
	}

	fmt.Println("signal caught - exiting")
	<-c.Done() // Wait for clean shutdown (cancelling the context triggered the shutdown)
}
```

See the [other examples](https://github.com/eclipse/paho.golang/tree/master/autopaho/examples) for further information on usage.

## QOS 1 & 2

QOS 1 & 2 provide assurances that messages will be delivered. To implement this a [session state](https://docs.oasis-open.org/mqtt/mqtt/v5.0/os/mqtt-v5.0-os.html#_Toc3901230)
is required that holds information on messages that have not been fully acknowledged. By default `autopaho` holds this 
state in memory meaning that messages will not be lost following a reconnection, but may be lost if the program is 
restarted (a file-based store can be used to avoid this).   

A range of settings impact message delivery; if you want guaranteed delivery, then remember to:

* Use a unique, client ID (you need to ensue any subsequent connections use the same ID)
* Configure `CleanStartOnInitialConnection` and `SessionExpiryInterval` appropriately (e.g. `false`, `600`).
* Use file-based persistence if you wish the session to survive an application restart
* Specify QOS 1 or 2 when publishing/subscribing.

When subscribing at QOS1/2:
* Remember that messages will not be queued until after the initial `Subscribe` call.
* If you subscribed previously (and the session is live) then expect to receive messages upon connection (you do not need
to call `Subscribe` when reconnecting; however this is recommended in case the session was lost).

`example/docker` provides a demonstration of how this can work. You can confirm this yourself using two terminal windows:

| Terminal1                 | Terminal2                                                                                                 |
|---------------------------|-----------------------------------------------------------------------------------------------------------|
| `docker compose up -d`    |                                                                                                           |
|                           | `docker compose logs --follow`                                                                            |
|                           | Wait until you see the subscriber receiving messages (e.g. `docker-sub-1  received message: {"Count":1}`) |
| `docker compose stop sub` |                                                                                                           |
| Wait 20 seconds           |                                                                                                           |
| `docker compose up -d`    |                                                                                                           |
|                           | Verify that `sub` received all messages despite the stop/start.                                           |

Note: The logs can be easier fo follow if you comment out the `log_type all` in `mosquitto.conf`.

## Queue

When publishing a message, there are a number of things that can go wrong; for example:
* The connection to the server may drop (or not have even come up before your initial message is ready)
* The application might be restarted (but you still want messages previously published to be delivered)
* `ConnectionManager.Publish` may timeout because you are attempting to publish a lot of messages in a short space of time.

With MQTT v3.1 this was generally handled by adding messages to the session; meaning they would be retried if the 
connection droped and was reestablished. MQTT v5 introduces a [Receive Maximum](https://docs.oasis-open.org/mqtt/mqtt/v5.0/os/mqtt-v5.0-os.html#_Toc3901083)
which limits the number of messages that can be in flight (and, hence, in the session).

`ConnectionManager.PublishViaQueue` provides a solution; messages passed to this function are added to a queue and 
transmitted when possible. By default, this queue is held in memory but you can use an alternate `ClientConfig.Queue`
(e.g. `queue/disk`) if you wish the queue to survive an application restart.

See `examples/queue`.

## Outgoing publish events

`paho.ClientConfig.PublishEvents` provides callbacks that allow you to track (and edit) packets relating to outgoing 
publications. This was added to provide a way to track message delivery but may be useful 

| Callback | Behaviour |
| --- | --- |
| `BeforeSend` | Before each PUBLISH/PUBREL write, including QoS 0 and retransmissions. Allows changes to the packet that is to be transmitted. |
| `PreparePubrel` | On the first successful PUBREC, before storing PUBREL. Allows properties to be copied from PUBLISH into PUBREL. |
| `OnEvent` | Reports acknowledgements and session loss through the notifications below. |

`ReadStoredPacket` is false by default. Enable it to load stored source packets
for `PreparePubrel` and `OnEvent`; `BeforeSend` always has its source available.

| Notification | Trigger | Meaning |
| --- | --- | --- |
| `session.PublishEventPubrec` | Successful PUBREC, including repeats | Progress in a QoS 2 exchange. |
| `session.PublishEventComplete` | PUBACK, rejection PUBREC, or PUBCOMP | The transaction completed; inspect the reason code to determine the outcome. |
| `session.PublishEventSessionLost` | CONNACK, DISCONNECT, or none | The session was cleared and the publication discarded; delivery is unknown. |

See the [delivery extension](../paho/extensions/delivery/readme.md) for examples,
application-ID tracking, and detailed guidance on outcomes, errors, and persistence.
