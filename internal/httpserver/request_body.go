package httpserver

import (
	"errors"
	"io"
)

// ErrRequestBodyTooLarge identifies a request body that exceeds its limit.
var ErrRequestBodyTooLarge = errors.New("request body exceeds maximum size")

// LimitedBodyReader is the streaming size-limit capability required by the
// public server adapter SPI. It remains separate from HandlerContext so
// internal protocol tests can use lightweight stubs.
type LimitedBodyReader interface {
	RequestBodyLimited(maxBytes int64) ([]byte, error)
}

// ReadRequestBody reads a body with an optional maximum size. Non-positive
// maxBytes means unlimited.
func ReadRequestBody(ctx HandlerContext, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return ctx.RequestBody()
	}
	limited, ok := ctx.(LimitedBodyReader)
	if !ok {
		body, err := ctx.RequestBody()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > maxBytes {
			return nil, ErrRequestBodyTooLarge
		}
		return body, nil
	}
	return limited.RequestBodyLimited(maxBytes)
}

// readBodyLimited reads at most maxBytes+1 bytes, using the extra byte only
// to distinguish an exactly-at-limit body from an oversized body.
func readBodyLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes == int64(^uint64(0)>>1) {
		return io.ReadAll(r)
	}
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}
	return body, nil
}
