/*
 * Copyright (c) 2024 Contributors to the Eclipse Foundation
 *
 *  All rights reserved. This program and the accompanying materials
 *  are made available under the terms of the Eclipse Public License v2.0
 *  and Eclipse Distribution License v1.0 which accompany this distribution.
 *
 * The Eclipse Public License is available at
 *    https://www.eclipse.org/legal/epl-2.0/
 *  and the Eclipse Distribution License is available at
 *    http://www.eclipse.org/org/documents/edl-v10.php.
 *
 *  SPDX-License-Identifier: EPL-2.0 OR BSD-3-Clause
 */

package autopaho

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho/queue"
	"github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session/state"
	"github.com/gorilla/websocket"

	"github.com/eclipse/paho.golang/paho"
)

// AutoPaho is a wrapper around github.com/eclipse/paho.golang that simplifies the connection process; it automates
// connections (retrying until the connection comes up) and will attempt to re-establish the connection if it is lost.
//
// The aim is to cover a common requirement (connect to the server and try to keep the connection up); if your
// requirements differ then please consider using github.com/eclipse/paho.golang directly (perhaps using the
// code in this file as a base; a secondary aim is to provide example code!).

// ConnectionDownError Down will be returned when a request is made but the connection to the server is down
// Note: It is possible that the connection will drop between the request being made and a response being received, in
// which case a different error will be received (this is only returned if the connection is down at the time the
// request is made).
var ConnectionDownError = errors.New("connection with the MQTT server is currently down")

// WebSocketConfig enables customisation of the websocket connection
type WebSocketConfig struct {
	Dialer func(url *url.URL, tlsCfg *tls.Config) *websocket.Dialer // If non-nil this will be called before each websocket connection (allows full configuration of the dialer used)
	Header func(url *url.URL, tlsCfg *tls.Config) http.Header       // If non-nil this will be called before each connection attempt to get headers to include with request
}

type PublishReceived struct {
	paho.PublishReceived
	ConnectionManager *ConnectionManager
}

// ClientConfig adds a few values, required to manage the connection, to the standard paho.ClientConfig (note that
// conn will be ignored)
type ClientConfig struct {
	ServerUrls                    []*url.URL  // URL(s) for the MQTT server (schemes supported include 'mqtt' and 'tls')
	TlsCfg                        *tls.Config // Configuration used when connecting using TLS
	KeepAlive                     uint16      // Keepalive period in seconds (the maximum time interval that is permitted to elapse between the point at which the Client finishes transmitting one MQTT Control Packet and the point it starts sending the next)
	CleanStartOnInitialConnection bool        //  Clean Start flag, if true, existing session information will be cleared on the first connection (it will be false for subsequent connections)
	SessionExpiryInterval         uint32      // Session Expiry Interval in seconds (if 0 the Session ends when the Network Connection is closed)

	// Deprecated: ConnectRetryDelay is deprecated and its functionality is replaced by ReconnectBackoff.
	ConnectRetryDelay time.Duration           // How long to wait between connection attempts (defaults to 10s)
	ReconnectBackoff  func(int) time.Duration // How long to wait after failed connection attempt N (defaults to 10s)
	ConnectTimeout    time.Duration           // How long to wait for the connection process to complete (defaults to 10s)
	WebSocketCfg      *WebSocketConfig        // Enables customisation of the websocket connection

	Queue queue.Queue // Used to queue up publish messages (if nil an error will be returned if publish could not be transmitted)

	// Depreciated: Use ServerUrls instead (this will be used if ServerUrls is empty). Will be removed in a future release.
	BrokerUrls []*url.URL

	// AttemptConnection, if provided, will be called to establish a network connection.
	// The returned `conn` must support thread safe writing; most wrapped net.Conn implementations like tls.Conn
	// are not thread safe for writing.
	// To fix, use packets.NewThreadSafeConn wrapper or extend the custom net.Conn struct with sync.Locker.
	AttemptConnection func(context.Context, ClientConfig, *url.URL) (net.Conn, error)

	OnConnectionUp   func(*ConnectionManager, *paho.Connack) // Called when a connection is made (including reconnection). Connection Manager passed to simplify subscriptions. Supplied function must not block.
	OnConnectionDown func() bool                             // Only called after the connection that resulted in OnConnectionUp is dropped. Returning false will cause autopaho to cease attempting to connect. Supplied function must not block.
	OnConnectError   func(error)                             // Called (within a goroutine) whenever a connection attempt fails. Will wrap autopaho.ConnackError on server deny.

	Debug      log.Logger // By default set to NOOPLogger{},set to a logger for debugging info
	Errors     log.Logger // By default set to NOOPLogger{},set to a logger for errors
	PahoDebug  log.Logger // debugger passed to the paho package (will default to NOOPLogger{})
	PahoErrors log.Logger // error logger passed to the paho package (will default to NOOPLogger{})

	ConnectUsername string
	ConnectPassword []byte

	WillMessage    *paho.WillMessage
	WillProperties *paho.WillProperties

	ConnectPacketBuilder func(*paho.Connect, *url.URL) (*paho.Connect, error) // called prior to connection allowing customisation of the CONNECT packet

	// DisconnectPacketBuilder - called prior to disconnection allowing customisation of the DISCONNECT
	// packet. If the function returns nil, then no DISCONNECT packet will be passed; if nil a default packet is sent.
	DisconnectPacketBuilder func() *paho.Disconnect

	// We include the full paho.ClientConfig in order to simplify moving between the two packages.
	// Note that Conn will be ignored.
	paho.ClientConfig
}

// ConnectionManager manages the connection with the server and provides the ability to publish messages
type ConnectionManager struct {
	cli      *paho.Client  // The client will only be set when the connection is up (only updated within NewServerConnection goRoutine)
	connUp   chan struct{} // Channel is closed when the connection is up (only valid if cli == nil; must lock Mu to read)
	connDown chan struct{} // Channel is closed when the connection is down (only valid if cli != nil; must lock Mu to read)
	mu       sync.Mutex    // protects all of the above

	cfg       ClientConfig       // The config passed to NewConnection (stored to enable getters)
	cancelCtx context.CancelFunc // Calling this will shut things down cleanly

	queue   queue.Queue    // In not nil, this will be used to queue publish requests
	queueWg sync.WaitGroup // Waits on goroutine that monitors Queue

	done chan struct{} // Channel that will be closed when the process has cleanly shutdown

	debug  log.Logger // By default set to NOOPLogger{},set to a logger for debugging info
	errors log.Logger // By default set to NOOPLogger{},set to a logger for errors
}

// ResetUsernamePassword clears any configured username and password on the client configuration
//
// Set ConnectUsername and ConnectPassword directly instead.
func (cfg *ClientConfig) ResetUsernamePassword() {
	cfg.ConnectPassword = []byte{}
	cfg.ConnectUsername = ""
}

// SetUsernamePassword configures username and password properties for the Connect packets
// These values are staged in the ClientConfig, and preparation of the Connect packet is deferred.
//
// Deprecated: Set ConnectUsername and ConnectPassword directly instead.
func (cfg *ClientConfig) SetUsernamePassword(username string, password []byte) {
	if len(username) > 0 {
		cfg.ConnectUsername = username
	}

	if len(password) > 0 {
		cfg.ConnectPassword = password
	}
}

// SetWillMessage configures the Will topic, payload, QOS and Retain facets of the client connection
// These values are staged in the ClientConfig, for later preparation of the Connect packet.
//
// Deprecated: Set WillMessage and WillProperties directly instead.
func (cfg *ClientConfig) SetWillMessage(topic string, payload []byte, qos byte, retain bool) {
	cfg.WillMessage = &paho.WillMessage{
		Retain:  retain,
		Payload: payload,
		Topic:   topic,
		QoS:     qos,
	}

	// Default Will Properties will match the values used in previous versions for compatibility
	willDelayInterval := uint32(2 * cfg.KeepAlive)

	cfg.WillProperties = &paho.WillProperties{
		// Most of these are nil/empty or defaults until related methods are exposed for configuration
		WillDelayInterval: &willDelayInterval,
	}
}

// SetConnectPacketConfigurator assigns a callback for modification of the Connect packet, called before the connection is opened, allowing the application to adjust its configuration before establishing a connection.
// This function should be treated as asynchronous, and expected to have no side effects.
//
// Deprecated: Set ConnectPacketBuilder directly instead. This function exists for
// backwards compatibility only (and may be removed in the future).
func (cfg *ClientConfig) SetConnectPacketConfigurator(fn func(*paho.Connect) (*paho.Connect, error)) bool {
	cfg.ConnectPacketBuilder = func(pc *paho.Connect, u *url.URL) (*paho.Connect, error) {
		return fn(pc)
	}
	return fn != nil
}

// SetDisConnectPacketConfigurator assigns a callback for the provision of a DISCONNECT packet. By default, a DISCONNECT
// is sent to the server when Disconnect is called; setting a callback allows a custom packet to be provided, or no
// packet (by returning nil).
//
// Deprecated: Set DisconnectPacketBuilder directly instead. This function exists for
// backwards compatibility only (and may be removed in the future).
func (cfg *ClientConfig) SetDisConnectPacketConfigurator(fn func() *paho.Disconnect) {
	cfg.DisconnectPacketBuilder = fn
}

// buildConnectPacket constructs a Connect packet for the paho client, based on staged configuration.
// If the program uses SetConnectPacketConfigurator, the provided callback will be executed with the preliminary Connect packet representation.
func (cfg *ClientConfig) buildConnectPacket(firstConnection bool, serverURL *url.URL) (*paho.Connect, error) {

	cp := &paho.Connect{
		KeepAlive:  cfg.KeepAlive,
		ClientID:   cfg.ClientID,
		CleanStart: cfg.CleanStartOnInitialConnection && firstConnection,
	}

	if len(cfg.ConnectUsername) > 0 {
		cp.UsernameFlag = true
		cp.Username = cfg.ConnectUsername
	}

	if len(cfg.ConnectPassword) > 0 {
		cp.PasswordFlag = true
		cp.Password = cfg.ConnectPassword
	}

	if cfg.WillMessage != nil {
		cp.WillMessage = cfg.WillMessage
		if cfg.WillProperties != nil {
			cp.WillProperties = cfg.WillProperties
		} else {
			cp.WillProperties = &paho.WillProperties{}
		}
	}

	if cfg.SessionExpiryInterval != 0 {
		cp.Properties = &paho.ConnectProperties{SessionExpiryInterval: &cfg.SessionExpiryInterval}
	}

	if cfg.ConnectPacketBuilder != nil {
		var err error
		cp, err = cfg.ConnectPacketBuilder(cp, serverURL)
		if err != nil {
			return nil, err
		}
	}

	return cp, nil
}

// NewConnection creates a connection manager and begins the connection process (will retry until the context is cancelled)
func NewConnection(ctx context.Context, cfg ClientConfig) (*ConnectionManager, error) {
	if cfg.Debug == nil {
		cfg.Debug = log.NOOPLogger{}
	}
	if cfg.Errors == nil {
		cfg.Errors = log.NOOPLogger{}
	}
	if cfg.ReconnectBackoff == nil {
		// for backwards compatibility we check for ConnectRetryDelay first
		// before using the default constant backoff strategy (which behaves
		// identically to the previous behaviour)
		if cfg.ConnectRetryDelay == 0 {
			cfg.ReconnectBackoff = NewConstantBackoff(10 * time.Second)
		} else {
			cfg.ReconnectBackoff = NewConstantBackoff(cfg.ConnectRetryDelay)
		}
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if len(cfg.ServerUrls) == 0 { // backwards compatibility
		cfg.ServerUrls = cfg.BrokerUrls
	}
	if len(cfg.ServerUrls) == 0 { // This would cause an infinite loop
		return nil, errors.New("no server urls provided")
	}
	if cfg.Queue == nil {
		cfg.Queue = memory.New()
	}
	if cfg.Session == nil { // Must create this, or it will be recreated upon reconnection, and we will lose the session info
		cfg.Session = state.NewInMemory()
	}
	innerCtx, cancel := context.WithCancel(ctx)
	c := ConnectionManager{
		cli:       nil,
		connUp:    make(chan struct{}),
		cfg:       cfg,
		cancelCtx: cancel,
		queue:     cfg.Queue,
		done:      make(chan struct{}),
		errors:    cfg.Errors,
		debug:     cfg.Debug,
	}
	errChan := make(chan error, 1) // Will be sent one, and only one error per connection (buffered to prevent deadlock)
	firstConnection := true        // Set to false after we have successfully connected

	go func() {
		defer func() {
			c.queueWg.Wait() // Separate goroutine handling queue may be running
			close(c.done)
		}()

	mainLoop:
		for {
			// Error handler is used to guarantee that a single error will be received whenever the connection is lost
			eh := errorHandler{
				debug:                  cfg.Debug,
				mu:                     sync.Mutex{},
				errChan:                errChan,
				userOnClientError:      cfg.OnClientError,
				userOnServerDisconnect: cfg.OnServerDisconnect,
			}
			cliCfg := cfg
			cliCfg.OnClientError = eh.onClientError
			cliCfg.OnServerDisconnect = eh.onServerDisconnect
			cli, connAck := establishServerConnection(innerCtx, cliCfg, firstConnection)
			if cli == nil {
				break mainLoop // Only occurs when context is cancelled
			}

			c.mu.Lock()
			c.cli = cli
			c.connDown = make(chan struct{})
			close(c.connUp)
			c.mu.Unlock()

			if cfg.OnConnectionUp != nil {
				cfg.OnConnectionUp(&c, connAck)
			}

			if firstConnection {
				c.queueWg.Add(1)
				go func(ctx context.Context) {
					_ = c.managePublishQueue(ctx)
					c.queueWg.Done()
				}(innerCtx)
				firstConnection = false
			}

			var err error
			select {
			case err = <-errChan: // Message on the error channel indicates the connection has, or will, drop.
			case <-innerCtx.Done():
				cfg.Debug.Println("innerCtx Done")
				eh.shutdown() // Prevent any errors triggered by closure of context from reaching user
				// As the connection is up, we call disconnect to shut things down cleanly
				dp := &paho.Disconnect{ReasonCode: 0}
				if cfg.DisconnectPacketBuilder != nil {
					dp = cfg.DisconnectPacketBuilder()
				}
				if dp != nil {
					if err = c.cli.Disconnect(dp); err != nil {
						cfg.Debug.Printf("mainLoop: disconnect returned error: %s\n", err)
					}
				}
				if ctx.Err() != nil { // If this is due to outer context being cancelled, then this will have happened before the inner one gets cancelled.
					cfg.Debug.Printf("mainLoop: server connection handler exiting due to context: %s\n", ctx.Err())
				} else {
					cfg.Debug.Printf("mainLoop: server connection handler exiting due to Disconnect call: %s\n", innerCtx.Err())
				}
				break mainLoop
			}
			<-cli.Done() // Wait for the client to fully shutdown
			c.mu.Lock()
			c.cli = nil
			close(c.connDown)
			c.connUp = make(chan struct{})
			c.mu.Unlock()

			if cfg.OnConnectionDown != nil && !cfg.OnConnectionDown() {
				cfg.Debug.Printf("mainLoop: connection to server lost (%s); OnConnectionDown aborts reconnect\n", err)
				break mainLoop
			}
			cfg.Debug.Printf("mainLoop: connection to server lost (%s); will reconnect\n", err)
		}
		cfg.Debug.Println("mainLoop: connection manager has terminated")
	}()
	return &c, nil
}

// Disconnect closes the connection (if one is up) and shuts down any active processes before returning
func (c *ConnectionManager) Disconnect(ctx context.Context) error {
	c.cancelCtx()
	select {
	case <-c.done: // wait for goroutine to exit
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns a channel that will be closed when the connection handler has shutdown cleanly
// Note: We cannot currently tell when the mqtt has fully shutdown (so it may still be in the process of closing down)
func (c *ConnectionManager) Done() <-chan struct{} {
	return c.done
}

// AwaitConnection will return when the connection comes up or the context is cancelled (only returns an error
// if context is cancelled). If you require more complex connection management then consider using the OnConnectionUp
// callback.
func (c *ConnectionManager) AwaitConnection(ctx context.Context) error {
	c.mu.Lock()
	ch := c.connUp
	c.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done: // If connection process is cancelled we should exit
		return fmt.Errorf("connection manager shutting down")
	}
}

// Authenticate is used to initiate a reauthentication of credentials with the
// server. This function sends the initial Auth packet to start the reauthentication
// then relies on the client AuthHandler managing any further requests from the
// server until either a successful Auth packet is passed back, or a Disconnect
// is received.
func (c *ConnectionManager) Authenticate(ctx context.Context, a *paho.Auth) (*paho.AuthResponse, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()

	if cli == nil {
		return nil, ConnectionDownError
	}
	return cli.Authenticate(ctx, a)
}

// Subscribe is used to send a Subscription request to the MQTT server.
// It is passed a pre-prepared Subscribe packet and blocks waiting for
// a response Suback, or for the timeout to fire. Any response Suback
// is returned from the function, along with any errors.
func (c *ConnectionManager) Subscribe(ctx context.Context, s *paho.Subscribe) (*paho.Suback, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()

	if cli == nil {
		return nil, ConnectionDownError
	}
	return cli.Subscribe(ctx, s)
}

// Unsubscribe is used to send an Unsubscribe request to the MQTT server.
// It is passed a pre-prepared Unsubscribe packet and blocks waiting for
// a response Unsuback, or for the timeout to fire. Any response Unsuback
// is returned from the function, along with any errors.
func (c *ConnectionManager) Unsubscribe(ctx context.Context, u *paho.Unsubscribe) (*paho.Unsuback, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()

	if cli == nil {
		return nil, ConnectionDownError
	}
	return cli.Unsubscribe(ctx, u)
}

// Publish is used to send a publication to the MQTT server.
// It is passed a pre-prepared `PUBLISH` packet and blocks waiting for the appropriate response,
// or for the timeout to fire.
// Any response message is returned from the function, along with any errors.
func (c *ConnectionManager) Publish(ctx context.Context, p *paho.Publish) (*paho.PublishResponse, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()

	if cli == nil {
		return nil, ConnectionDownError
	}
	return cli.Publish(ctx, p)
}

// QueuePublish holds info required to publish a message. A separate struct is used so options can be added in the future
// without breaking existing code
type QueuePublish struct {
	*paho.Publish
}

// PublishViaQueue is used to send a publication to the MQTT server via a queue (by default memory based).
// An error will be returned if the message could not be added to the queue, otherwise the message will be delivered
// in the background with no status updates available.
// Use this function when you wish to rely upon the libraries best-effort to transmit the message; it is anticipated
// that this will generally be in situations where the network link or power supply is unreliable.
// Messages will be written to a queue (configuring a disk-based queue is recommended) and transmitted where possible.
// To maximise the chance of a successful delivery:
//   - Leave CleanStartOnInitialConnection set to false
//   - Set SessionExpiryInterval such that sessions will outlive anticipated outages (this impacts inflight messages only)
//   - Set ClientConfig.Session to a session manager with persistent storage
//   - Set ClientConfig.Queue to a queue with persistent storage
func (c *ConnectionManager) PublishViaQueue(ctx context.Context, p *QueuePublish) error {
	var b bytes.Buffer
	if _, err := p.Packet().WriteTo(&b); err != nil {
		return err
	}
	_, err := c.queue.Enqueue(&b)
	return err
}

// TerminateConnectionForTest closes the active connection (if any). This function is intended for testing only, it
// simulates connection loss which supports testing QOS1 and 2 message delivery.
func (c *ConnectionManager) TerminateConnectionForTest() {
	c.mu.Lock()
	if c.cli != nil {
		c.cli.TerminateConnectionForTest()
	}
	c.mu.Unlock()
}

// AddOnPublishReceived adds a function that will be called when a PUBLISH is received
// The new function will be called after any functions already in the list
// Returns a function that can be called to remove the callback
func (c *ConnectionManager) AddOnPublishReceived(f func(PublishReceived) (bool, error)) func() {
	// Removing a handler is a bit tricky because the connection may have dropped and been reestablished
	// The current approach is to leave the handler in place but turn it into a no-op (this could be improved!)
	isActive := true
	fn := func(pr paho.PublishReceived) (bool, error) {
		if !isActive {
			return false, nil
		}
		return f(PublishReceived{
			PublishReceived:   pr,
			ConnectionManager: c,
		})
	}
	var removeFromClient func()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil {
		removeFromClient = c.cli.AddOnPublishReceived(fn)
	}

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if removeFromClient != nil {
			removeFromClient()
		}
		isActive = false
	}
}

// managePublishQueue sends messages from the publish queue.
// blocks until the context is cancelled.
func (c *ConnectionManager) managePublishQueue(ctx context.Context) error {
connectionLoop:
	for {
		c.debug.Println("queue AwaitConnection")
		if err := c.AwaitConnection(ctx); err != nil {
			return err
		}

		c.debug.Println("queue got connection")
		c.mu.Lock()
		cli := c.cli
		connDown := c.connDown
		c.mu.Unlock()
		if cli == nil { // Possible connection dropped immediately
			continue
		}

	queueLoop:
		for {
			select {
			case <-ctx.Done():
				c.debug.Println("queue done")
				return ctx.Err()
			case <-connDown:
				c.debug.Println("connection down")
				continue connectionLoop
			case <-c.queue.Wait():
			}

			// Connection is up, and we have at least one thing to send
			for {
				entry, err := c.queue.Peek() // If this succeeds, we MUST call Remove, Quarantine or Leave
				if errors.Is(err, queue.ErrEmpty) {
					c.debug.Println("everything in queue transmitted")
					continue queueLoop
				} else if err != nil {
					// if Peek() keeps returning errors, we will loop forever.
					// see https://github.com/eclipse/paho.golang/issues/234
					c.errors.Printf("error retrieving queue entry: %s", err)
					time.Sleep(1 * time.Second)
					continue
				}
				_, r, err := entry.Reader()
				if err != nil {
					c.errors.Printf("error retrieving reader for queue entry: %s", err)
					if err := entry.Leave(); err != nil {
						c.errors.Printf("error leaving queue entry: %s", err)
					}
					time.Sleep(1 * time.Second)
					continue
				}

				p, err := packets.ReadPacket(r)
				if err != nil {
					c.errors.Printf("error retrieving packet from queue: %s", err)
					// If the packet cannot be processed, then we need to remove it from the queue
					// (ideally into an error queue).
					if err := entry.Quarantine(); err != nil {
						c.errors.Printf("error moving queue entry to quarantine: %s", err)
					}
					continue
				}

				pub, ok := p.Content.(*packets.Publish)
				if !ok {
					c.errors.Printf("packet from queue is not a Publish")
					if qErr := entry.Quarantine(); qErr != nil {
						c.errors.Printf("error moving queue entry to quarantine: %s", err)
					}
					continue
				}
				pub2 := paho.Publish{
					PacketID: 0,
					QoS:      pub.QoS,
					Retain:   pub.Retain,
					Topic:    pub.Topic,
					Payload:  pub.Payload,
				}
				pub2.InitProperties(pub.Properties)

				// PublishWithOptions using PublishMethod_AsyncSend will block until the packet has been transmitted
				// and then return (at this point any pub1+ publish will be in the session so will be retried)
				c.debug.Printf("publishing message from queue with topic %s", pub2.Topic)
				if _, err = cli.PublishWithOptions(ctx, &pub2, paho.PublishOptions{Method: paho.PublishMethod_AsyncSend}); err != nil {
					c.errors.Printf("error publishing from queue: %s", err)
					if errors.Is(err, paho.ErrInvalidArguments) { // Some errors should not be retried
						if err := entry.Remove(); err != nil {
							c.errors.Printf("error removing queue entry: %s", err)
						}
						// Need a way to notify the user of this
					} else if errors.Is(err, paho.ErrNetworkErrorAfterStored) { // Message in session so remove from queue
						if err := entry.Remove(); err != nil {
							c.errors.Printf("error removing queue entry: %s", err)
						}
					} else {
						if err := entry.Leave(); err != nil { // the message was not sent, so leave it in the queue
							c.errors.Printf("error leaving queue entry: %s", err)
						}
						time.Sleep(1 * time.Second)
					}

					// The error might be fatal (connection will drop) or could be temporary (i.e. PacketTimeout exceeded)
					// as a result we currently retry unless we know the connection has dropped, or it's time to exit
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-connDown:
						continue connectionLoop
					default: // retry
						continue
					}
				}
				if err := entry.Remove(); err != nil { // successfully published
					c.errors.Printf("error removing queue entry: %s", err)
					continue
				}
			}
		}
	}
}
