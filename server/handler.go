package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/websocket"

	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	headerUpgrade    = "Upgrade"
	upgradeWebSocket = "websocket"
)

// Handler returns a native Hertz handler for the remote ACP endpoint.
func (s *ACPServer) Handler() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		isWSUpgrade := strings.EqualFold(string(c.GetHeader(headerUpgrade)), upgradeWebSocket)
		switch {
		case c.IsGet() && isWSUpgrade:
			s.handleWebSocket(ctx, c)
		case isWSUpgrade:
			// WebSocket upgrade is only valid on GET per RFC 6455.
			acphttpserver.WriteHertzText(c, http.StatusBadRequest, "WebSocket upgrade requires GET method")
		default:
			s.serveHTTP(ctx, c)
		}
	}
}

func (s *ACPServer) serveHTTP(ctx context.Context, c *app.RequestContext) {
	hctx := acphttpserver.NewHertzHandlerContext(ctx, c)
	acphttpserver.ServeHTTPProtocol(hctx, s.protocolServer(), string(c.Method()))
}

// handleWebSocket upgrades the connection to WebSocket and serves it.
func (s *ACPServer) handleWebSocket(ctx context.Context, c *app.RequestContext) {
	wc, err := s.newWSConn(ctx)
	if err != nil {
		s.logger.CtxError(ctx, "create websocket connection failed: %v", err)
		acphttpserver.WriteHertzText(c, http.StatusInternalServerError, "failed to create connection")
		return
	}

	c.Response.Header.Set(acptransport.HeaderConnectionID, wc.id)
	err = s.upgrader.Upgrade(c, func(wsConn *websocket.Conn) {
		wc.Serve(ctx, wsConn)
		wc.Close()
	})
	if err != nil {
		wc.Close()
	}
}
