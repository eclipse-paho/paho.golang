# RouterContext Extension

⚠️ **EXPERIMENTAL - NOT PART OF CORE LIBRARY**

This extension is **experimental and not part of the paho.golang core library**. It is not covered by semantic versioning guarantees and may be broken by updates elsewhere in the codebase. Support for this extension is limited. Use at your own risk.

RouterContext is a context-aware message router extension for paho.golang that provides topic-based message routing with middleware support.

## Overview

The RouterContext package provides a `Router` implementation that allows you to:
- Route incoming MQTT messages to different handlers based on topic patterns
- Support MQTT wildcard patterns (`+` for single-level, `#` for multi-level)
- Add middleware to wrap handlers for cross-cutting concerns (logging, metrics, error handling)
- Handle messages with Go's `context.Context` for cancellation and timeout control

## Installation

RouterContext is part of the paho.golang package:

```go
import "github.com/eclipse/paho.golang/paho/extensions/routercontext"
```

## Basic Usage

### Creating a Router

```go
package main

import (
	"context"
	"log"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/routercontext"
)

func main() {
	// Create a new router instance
	router := routercontext.NewRouter()

	// Register handlers for specific topics
	router.RegisterHandler("sensors/temperature", func(ctx context.Context, p *paho.Publish) {
		log.Printf("Temperature reading: %s\n", string(p.Payload))
	})

	router.RegisterHandler("sensors/humidity", func(ctx context.Context, p *paho.Publish) {
		log.Printf("Humidity reading: %s\n", string(p.Payload))
	})

	// Set a default handler for unmatched messages
	router.DefaultHandler(func(ctx context.Context, p *paho.Publish) {
		log.Printf("Unmatched message on topic: %s\n", p.Topic)
	})
}
```

### Topic Patterns

RouterContext supports MQTT wildcard patterns:

```go
// Single-level wildcard: matches one level only
router.RegisterHandler("sensors/+/status", handler)
// Matches: sensors/temp/status, sensors/humidity/status
// Does NOT match: sensors/temp/zone1/status

// Multi-level wildcard: matches zero or more levels (must be at end)
router.RegisterHandler("sensors/#", handler)
// Matches: sensors/temp, sensors/temp/zone1, sensors/temp/zone1/building2

// Exact match
router.RegisterHandler("system/shutdown", handler)
// Matches only: system/shutdown
```

## Middleware

Middleware allows you to wrap handlers with additional functionality. Middleware are applied in the order they are registered.

### Creating Middleware

```go
// Logging middleware
func LoggingMiddleware(next routercontext.Handler) routercontext.Handler {
	return func(ctx context.Context, p *paho.Publish) {
		log.Printf("Handling message on topic: %s", p.Topic)
		next(ctx, p)
		log.Printf("Finished handling message on topic: %s", p.Topic)
	}
}

// Metrics middleware
func MetricsMiddleware(next routercontext.Handler) routercontext.Handler {
	return func(ctx context.Context, p *paho.Publish) {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			log.Printf("Handler execution time: %v", duration)
		}()
		next(ctx, p)
	}
}

// Error recovery middleware
func RecoveryMiddleware(next routercontext.Handler) routercontext.Handler {
	return func(ctx context.Context, p *paho.Publish) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Panic recovered: %v", r)
			}
		}()
		next(ctx, p)
	}
}
```

### Using Middleware

```go
router := routercontext.NewRouter()

// Register middleware (applied in order)
router.Use(LoggingMiddleware)
router.Use(MetricsMiddleware)
router.Use(RecoveryMiddleware)

// Register handlers (will be wrapped by all middleware)
router.RegisterHandler("sensors/#", sensorHandler)
```

Middleware execution flow:
```
Request ->
  LoggingMiddleware (before) ->
    MetricsMiddleware (before) ->
      RecoveryMiddleware (before) ->
        Handler
      RecoveryMiddleware (after)
    MetricsMiddleware (after)
  LoggingMiddleware (after)
-> Response
```

## Using with MQTT Client

### With autopaho (Automatic reconnection)

```go
import (
	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/routercontext"
)

func main() {
	router := routercontext.NewRouter()
	
	// Configure router
	router.RegisterHandler("temperature/#", tempHandler)
	router.RegisterHandler("pressure/#", pressureHandler)
	
	cfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{serverURL},
		ClientConfig: paho.ClientConfig{
			ClientID: "my-client",
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					router.Route(pr.Packet.Packet())
					return true, nil
				},
			},
		},
	}
	
	client, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		panic(err)
	}
	
	// Subscribe to topics
	client.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{
			{Topic: "temperature/#", QoS: 1},
			{Topic: "pressure/#", QoS: 1},
		},
	})
}
```

### With paho.Client (Manual connection management)

```go
func main() {
	router := routercontext.NewRouter()
	router.RegisterHandler("test/#", testHandler)
	
	serverConn, err := net.Dial("tcp", "localhost:1883")
	if err != nil {
		panic(err)
	}
	
	c := paho.NewClient(paho.ClientConfig{
		Conn: serverConn,
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){
			func(pr paho.PublishReceived) (bool, error) {
				router.Route(pr.Packet.Packet())
				return true, nil
			},
		},
	})
	
	// ... rest of client setup
}
```

## API Reference

### Router Methods

#### `NewRouter() *Router`
Creates and returns a new Router instance.

#### `RegisterHandler(topic string, handler Handler)`
Registers a handler for the given topic pattern. Multiple handlers can be registered for the same topic.

#### `UnregisterHandler(topic string)`
Removes all handlers registered for the given topic pattern.

#### `Use(middleware ...Middleware)`
Registers middleware to wrap all handlers. Middleware are applied in order.

#### `Route(pb *packets.Publish)`
Routes a Publish packet to all matching handlers. Called internally by the MQTT client.

#### `DefaultHandler(handler Handler)`
Sets a handler to be called for messages that don't match any registered topic pattern. Pass `nil` to unset.

#### `SetDebugLogger(logger log.Logger)`
Sets a logger for debug output from the router.

### Types

#### `Handler`
```go
type Handler func(context.Context, *paho.Publish)
```
A function that handles incoming MQTT messages with context support.

#### `Middleware`
```go
type Middleware func(Handler) Handler
```
A function that wraps a handler to add additional functionality.

## Examples

See the `autopaho/examples/routercontext/` directory for complete working examples including:
- Basic router usage
- Middleware implementation (logging and panic recovery)
- Context-aware message handling
- Integration with autopaho

## Performance Considerations

1. **Lock Contention**: The router uses `sync.RWMutex` for thread-safety. Most operations use read locks except for registration/unregistration.

2. **Topic Matching**: Topic matching involves string splitting and recursive pattern comparison. For high-throughput scenarios, prefer more specific topic patterns.

3. **Middleware Overhead**: Each middleware adds a function call overhead. Register only necessary middleware.

4. **Context Creation**: A background context is created for each message. For high-frequency scenarios, consider context pooling.

## Best Practices

1. **Quick Handlers**: Keep handlers fast; start goroutines for long-running operations.

2. **Error Handling**: Use middleware for centralized error handling and recovery.

3. **Avoid Deadlocks**: Don't call paho client functions from within handlers; use channels or goroutines instead.

4. **Topic Specificity**: Use specific topic patterns over `#` when possible for better performance.

5. **Middleware Ordering**: Order middleware from outer to inner concerns (logging → metrics → recovery → handler).

## Differences from paho.Router

The main differences between RouterContext and the standard `paho.StandardRouter`:

| Feature | StandardRouter | RouterContext |
|---------|----------------|---------------|
| Message Handler | `func(*Publish)` | `func(context.Context, *Publish)` |
| Middleware Support | No | Yes |
| Context Propagation | No | Yes |
| Cancellation Support | No | Yes (via context) |
| Timeout Control | No | Yes (via context) |
