package channels

import "context"

type Channel interface {
	Name() string
	Start(ctx context.Context, out chan<- InboundMessage, errs chan<- error)
	Send(ctx context.Context, msg OutboundMessage) error
	Close(ctx context.Context) error
}
