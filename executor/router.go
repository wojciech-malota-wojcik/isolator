package executor

import (
	"context"

	"github.com/pkg/errors"

	"github.com/outofforest/isolator/wire"
)

// HandlerFunc defines handler for content.
type HandlerFunc func(ctx context.Context, content interface{}, encode wire.EncoderFunc) error

// NewRouter returns new router.
func NewRouter() Router {
	return Router{
		handlers: map[string]HandlerFunc{},
	}
}

// Router maps content type to its handler.
type Router struct {
	handlers map[string]HandlerFunc
	types    []interface{}
}

// RegisterHandler registers new handler for the content type.
func (r Router) RegisterHandler(content interface{}, handler HandlerFunc) Router {
	contentType := wire.ContentToType(content)
	if _, exists := r.handlers[contentType]; exists {
		panic(errors.Errorf("handler for %s has been already registered", contentType))
	}

	r.handlers[contentType] = handler
	r.types = append(r.types, content)
	return r
}

// Handler returns handler for the content type.
func (r Router) Handler(content interface{}) (HandlerFunc, error) {
	contentType := wire.ContentToType(content)
	handler, exists := r.handlers[contentType]
	if !exists {
		return nil, errors.Errorf("handler for %s does not exist", contentType)
	}

	return handler, nil
}

// Types returns registered types.
func (r Router) Types() []interface{} {
	return r.types
}
