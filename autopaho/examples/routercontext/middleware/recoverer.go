package middleware

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/routercontext"
)

func Recoverer(next routercontext.Handler) routercontext.Handler {
	return func(ctx context.Context, p *paho.Publish) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("Recovered in f", r)
				debug.PrintStack()
			}
		}()

		next(ctx, p)
	}
}
