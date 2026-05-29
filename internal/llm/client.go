package llm

import "context"

type Client interface {
	CreateMessage(ctx context.Context, req Request) (Response, error)
}
