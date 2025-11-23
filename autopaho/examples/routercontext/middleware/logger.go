package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/eclipse/paho.golang/paho"
)

func Logger(next paho.MessageContextHandler) paho.MessageContextHandler {
	return func(ctx context.Context, p *paho.Publish) {
		start := time.Now()
		next(ctx, p)

		elapsed := time.Since(start)
		slog.InfoContext(ctx, "message procesed",
			slog.String("topic", p.Topic),
			slog.Int("packet_id", int(p.PacketID)),
			slog.Int("qos", int(p.QoS)),
			slog.Duration("latency", elapsed),
		)
	}
}
