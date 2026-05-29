package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"AIHelper/internal/channels"
)

type Channel struct {
	reader  *bufio.Scanner
	writer  io.Writer
	account string
	peer    string
	mu      sync.Mutex
}

func New(in io.Reader, out io.Writer) *Channel {
	return &Channel{
		reader:  bufio.NewScanner(in),
		writer:  out,
		account: "cli-local",
		peer:    "cli-user",
	}
}

func (c *Channel) Name() string {
	return "cli"
}

func (c *Channel) Start(ctx context.Context, out chan<- channels.InboundMessage, errs chan<- error) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		_, err := fmt.Fprint(c.writer, "You > ")
		c.mu.Unlock()
		if err != nil {
			sendError(ctx, errs, err)
			return
		}

		if !c.reader.Scan() {
			if err := c.reader.Err(); err != nil {
				sendError(ctx, errs, err)
			}
			return
		}

		text := strings.TrimSpace(c.reader.Text())
		msg := channels.InboundMessage{
			Text:      text,
			Channel:   c.Name(),
			AccountID: c.account,
			PeerID:    c.peer,
			SenderID:  c.peer,
		}
		select {
		case <-ctx.Done():
			return
		case out <- msg:
		}
	}
}

func (c *Channel) Send(ctx context.Context, msg channels.OutboundMessage) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := fmt.Fprintf(c.writer, "Assistant > %s\n", msg.Text)
	return err
}

func (c *Channel) Close(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func sendError(ctx context.Context, errs chan<- error, err error) {
	if err == nil || err == io.EOF {
		return
	}
	select {
	case <-ctx.Done():
	case errs <- err:
	default:
	}
}
