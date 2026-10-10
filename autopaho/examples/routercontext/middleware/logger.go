package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/routercontext"
)

func Logger(next routercontext.Handler) routercontext.Handler {
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
