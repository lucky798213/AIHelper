package delivery

import (
	"context"
	"fmt"
	"io"

	"AIHelper/internal/channels"
)

type Console struct {
	writer io.Writer
}

func NewConsole(writer io.Writer) *Console {
	return &Console{writer: writer}
}

func (d *Console) Deliver(ctx context.Context, msg channels.OutboundMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err := fmt.Fprintf(d.writer, "Assistant > %s\n", msg.Text)
	return err
}
