# RouterContext Example

⚠️ **EXPERIMENTAL - NOT PART OF CORE LIBRARY**

This example uses experimental extensions that are **not part of the paho.golang core library**. They are not covered by semantic versioning guarantees and may be broken by updates elsewhere in the codebase. Support is limited. Use at your own risk.

This example demonstrates how to use the context-aware router with autopaho for topic-based message routing with middleware support.

## Overview

AutoPaho provides the option to specify callbacks (`ClientConfig.OnPublishReceived`) that are called whenever a message is received. It's common for users to want multiple callbacks with the message topic determining which callback is called. The context router provides this functionality with added support for middleware and Go's `context.Context`.

## Creating a Router

First, create a context-aware router instance:

```go
router := routercontext.NewRouter()
```

## Configuring the MQTT Client

Configure `ClientConfig.OnPublishReceived` to route incoming messages through the router:

```go
autopaho.ClientConfig{
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
```

## Registering Handlers

Register handlers for specific topic patterns:

```go
// Exact topic match
router.RegisterHandler("test/test", func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("Received message on test/test: %s", string(p.Payload)))
})

// Single-level wildcard (+)
router.RegisterHandler("test/+/status", func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("Status update: %s", p.Topic))
})

// Multi-level wildcard (#)
router.RegisterHandler("test/test/#", func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("test/test/# received message with topic: %s", p.Topic))
})

// Default handler for unmatched topics
router.DefaultHandler(func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("Default handler received message with topic: %s", p.Topic))
})
```

## Using Middleware

Add middleware to wrap handlers for cross-cutting concerns like logging, metrics, or error handling:

```go
// Add logging middleware
router.Use(middleware.Logger)

// Add panic recovery middleware
router.Use(middleware.Recoverer)
```

Middleware are executed in the order they are registered, creating a chain around the handler.

## Managing Routes

Dynamically register and unregister handlers:

```go
// Register a new handler
router.RegisterHandler("sensors/temperature", func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("Temperature reading: %s", string(p.Payload)))
})

// Unregister a handler
router.UnregisterHandler("test/test/#")

// Update default handler
router.DefaultHandler(func(ctx context.Context, p *paho.Publish) {
    slog.InfoContext(ctx, fmt.Sprintf("New default handler: %s", p.Topic))
})
```

## Running the Example

```bash
go run main.go
```

The example will:
1. Connect to the Mosquitto test server
2. Subscribe to `test/#` topic
3. Register multiple handlers for different topic patterns
4. Apply middleware for logging and panic recovery
5. Publish test messages to demonstrate routing
6. Gracefully shutdown when receiving a message on `test/quit`

## Middleware Examples

The example includes two middleware implementations:

- **Logger**: Logs message handling with execution time
- **Recoverer**: Catches panics from handlers and logs them

See `middleware/` directory for implementations that can be used as templates for your own middleware.
