package httpserver

import "net/http"

const allowMethodsGetPostDelete = "GET, POST, DELETE"

// ServeProtocolMethod dispatches the shared ACP HTTP endpoint by method and
// applies a consistent 405 response for unsupported verbs.
func ServeProtocolMethod(ctx HandlerContext, method string, onPost, onGet, onDelete func()) {
	switch method {
	case http.MethodPost:
		if onPost != nil {
			onPost()
		}
	case http.MethodGet:
		if onGet != nil {
			onGet()
		}
	case http.MethodDelete:
		if onDelete != nil {
			onDelete()
		}
	default:
		ctx.SetResponseHeader("Allow", allowMethodsGetPostDelete)
		ctx.WriteError(http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ServeHTTPProtocol is a convenience that dispatches a single ACP Streamable
// HTTP request to the appropriate protocol handler. server.ACPServer uses this
// to avoid duplicating the dispatch + closure boilerplate.
func ServeHTTPProtocol(ctx HandlerContext, server ProtocolServer, method string) {
	ServeProtocolMethod(ctx, method,
		func() { HandleProtocolPost(ctx, server) },
		func() { HandleProtocolGet(ctx, server) },
		func() { HandleProtocolDelete(ctx, server) },
	)
}
