package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	hclient "github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/stream"
)

// wsStreamerFactory is a toy stream.StreamerFactory that dials the example
// AgentServer over WebSocket. Production users replace this with their own
// RPC-framework-specific dialer — the proxy only sees the interface.
type wsStreamerFactory struct {
	upstreamURL  string
	client       *hclient.Client
	upgrader     *websocket.ClientUpgrader
	writeTimeout time.Duration
}

var _ stream.StreamerFactory = (*wsStreamerFactory)(nil)

func newWSStreamerFactory(upstreamURL string) (*wsStreamerFactory, error) {
	c, err := hclient.NewClient(hclient.WithDialer(standard.NewDialer()))
	if err != nil {
		return nil, fmt.Errorf("create hertz client: %w", err)
	}
	return &wsStreamerFactory{
		upstreamURL:  upstreamURL,
		client:       c,
		upgrader:     &websocket.ClientUpgrader{},
		writeTimeout: 30 * time.Second,
	}, nil
}

func (f *wsStreamerFactory) NewStreamer(ctx context.Context, meta map[string]string) (stream.Streamer, error) {
	req := protocol.AcquireRequest()
	resp := protocol.AcquireResponse()

	httpURL := wsURLToHTTP(f.upstreamURL)
	req.SetRequestURI(httpURL)
	req.SetMethod(consts.MethodGet)
	for k, v := range meta {
		req.Header.Set(k, v)
	}
	f.upgrader.PrepareRequest(req)

	if err := f.client.Do(ctx, req, resp); err != nil {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
		return nil, fmt.Errorf("upstream dial: %w", err)
	}

	conn, err := f.upgrader.UpgradeResponse(req, resp)
	if err != nil {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
		return nil, fmt.Errorf("upstream ws upgrade: %w", err)
	}

	// req/resp are retained by the websocket connection's underlying reader;
	// the streamer takes ownership and does not release them explicitly —
	// they are short-lived compared to the streaming conn.
	return newWSStreamer(conn, f.writeTimeout), nil
}

// wsURLToHTTP converts ws:// / wss:// schemes into http:// / https:// for
// the hertz HTTP client dial step (the upgrade reuses the same TCP conn).
func wsURLToHTTP(u string) string {
	switch {
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	default:
		return u
	}
}
