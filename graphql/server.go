package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsarahq/thunder/batch"
	"github.com/samsarahq/thunder/diff"
	"github.com/samsarahq/thunder/reactive"
)

const (
	MaxSubscriptions = 200
	MinRerunInterval = 5 * time.Second
)

type JSONSocket interface {
	ReadJSON(value interface{}) error
	WriteJSON(value interface{}) error
	Close() error
}

type MakeCtxFunc func(context.Context) context.Context

type GraphqlLogger interface {
	StartExecution(ctx context.Context, tags map[string]string, initial bool)
	FinishExecution(ctx context.Context, tags map[string]string, delay time.Duration)
	Error(ctx context.Context, err error, tags map[string]string)
}

type conn struct {
	writeMu sync.Mutex
	socket  JSONSocket

	schema         *Schema
	mutationSchema *Schema
	ctx            context.Context
	makeCtx        MakeCtxFunc
	logger         GraphqlLogger
	middlewares    []MiddlewareFunc

	url string

	mutateMu sync.Mutex

	mu            sync.Mutex
	subscriptions map[string]*reactive.Rerunner
}

type InEnvelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

type OutEnvelope struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type"`
	Message  interface{}            `json:"message,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type subscribeMessage struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type mutateMessage struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type SanitizedError interface {
	error
	SanitizedError() string
}

type SafeError struct {
	message string
}

type ClientError SafeError

func (e ClientError) Error() string {
	return e.message
}

func (e ClientError) SanitizedError() string {
	return e.message
}

func (e SafeError) Error() string {
	return e.message
}

func (e SafeError) SanitizedError() string {
	return e.message
}

func NewClientError(format string, a ...interface{}) error {
	return ClientError{message: fmt.Sprintf(format, a...)}
}

func NewSafeError(format string, a ...interface{}) error {
	return SafeError{message: fmt.Sprintf(format, a...)}
}

func sanitizeError(err error) string {
	if sanitized, ok := err.(SanitizedError); ok {
		return sanitized.SanitizedError()
	}
	return "Internal server error"
}

func isCloseError(err error) bool {
	_, ok := err.(*websocket.CloseError)
	return ok || err == websocket.ErrCloseSent
}

func (c *conn) writeOrClose(out OutEnvelope) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.socket.WriteJSON(out); err != nil {
		if !isCloseError(err) {
			c.socket.Close()
			log.Printf("socket.WriteJSON: %s\n", err)
		}
	}
}

func mustMarshalJson(v interface{}) string {
	bytes, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(bytes)
}

func (c *conn) handleSubscribe(id string, subscribe *subscribeMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.subscriptions[id]; ok {
		return NewSafeError("duplicate subscription")
	}

	if len(c.subscriptions)+1 > MaxSubscriptions {
		return NewSafeError("too many subscriptions")
	}

	tags := map[string]string{"url": c.url, "query": subscribe.Query, "queryVariables": mustMarshalJson(subscribe.Variables), "id": id}

	query, err := Parse(subscribe.Query, subscribe.Variables)
	if query != nil {
		tags["queryType"] = query.Kind
		tags["queryName"] = query.Name
	}
	if err != nil {
		c.logger.Error(c.ctx, err, tags)
		return err
	}
	if err := PrepareQuery(c.schema.Query, query.SelectionSet); err != nil {
		c.logger.Error(c.ctx, err, tags)
		return err
	}

	var previous interface{}

	e := Executor{}

	initial := true
	c.subscriptions[id] = reactive.NewRerunner(c.ctx, func(ctx context.Context) (interface{}, error) {
		ctx = c.makeCtx(ctx)
		ctx = batch.WithBatching(ctx)

		start := time.Now()

		c.logger.StartExecution(ctx, tags, initial)

		var middlewares []MiddlewareFunc
		middlewares = append(middlewares, c.middlewares...)
		middlewares = append(middlewares, func(input *ComputationInput, next MiddlewareNextFunc) *ComputationOutput {
			output := next(input)
			output.Current, output.Error = e.Execute(input.Ctx, c.schema.Query, nil, input.ParsedQuery)
			return output
		})

		output := runMiddlewares(middlewares, &ComputationInput{
			Ctx:         ctx,
			Id:          id,
			ParsedQuery: query,
			Previous:    previous,
			Query:       subscribe.Query,
			Variables:   subscribe.Variables,
		})
		current, err := output.Current, output.Error

		c.logger.FinishExecution(ctx, tags, time.Since(start))

		if err != nil {
			if extractPathError(err) == context.Canceled {
				go c.closeSubscription(id)
				return nil, err
			}

			if !initial {
				// If this a re-computation, tell the Rerunner to retry the computation
				// without dumping the contents of the current computation cache.
				// Note that we are swallowing the propagation of the error in this case,
				// but we still log it.
				if _, ok := err.(SanitizedError); !ok {
					extraTags := map[string]string{"retry": "true"}
					for k, v := range tags {
						extraTags[k] = v
					}
					c.logger.Error(ctx, err, extraTags)
				}

				return nil, reactive.RetrySentinelError
			}

			c.writeOrClose(OutEnvelope{
				ID:       id,
				Type:     "error",
				Message:  sanitizeError(err),
				Metadata: output.Metadata,
			})
			go c.closeSubscription(id)

			if _, ok := err.(SanitizedError); !ok {
				c.logger.Error(ctx, err, tags)
			}
			return nil, err
		}

		d := diff.Diff(previous, current)
		previous = current
		initial = false

		if initial || d != nil {
			c.writeOrClose(OutEnvelope{
				ID:       id,
				Type:     "update",
				Message:  d,
				Metadata: output.Metadata,
			})
		}

		return nil, nil
	}, MinRerunInterval)

	return nil
}

func (c *conn) handleMutate(id string, mutate *mutateMessage) error {
	// TODO: deduplicate code
	c.mu.Lock()
	defer c.mu.Unlock()

	tags := map[string]string{"url": c.url, "query": mutate.Query, "queryVariables": mustMarshalJson(mutate.Variables), "id": id}

	query, err := Parse(mutate.Query, mutate.Variables)
	if query != nil {
		tags["queryType"] = query.Kind
		tags["queryName"] = query.Name
	}
	if err != nil {
		c.logger.Error(c.ctx, err, tags)
		return err
	}
	if err := PrepareQuery(c.mutationSchema.Mutation, query.SelectionSet); err != nil {
		c.logger.Error(c.ctx, err, tags)
		return err
	}

	e := Executor{}
	c.subscriptions[id] = reactive.NewRerunner(c.ctx, func(ctx context.Context) (interface{}, error) {
		// Serialize all mutates for a given connection.
		c.mutateMu.Lock()
		defer c.mutateMu.Unlock()

		ctx = c.makeCtx(ctx)
		ctx = batch.WithBatching(ctx)

		start := time.Now()
		c.logger.StartExecution(ctx, tags, true)

		var middlewares []MiddlewareFunc
		middlewares = append(middlewares, c.middlewares...)
		middlewares = append(middlewares, func(input *ComputationInput, next MiddlewareNextFunc) *ComputationOutput {
			output := next(input)
			output.Current, output.Error = e.Execute(input.Ctx, c.mutationSchema.Mutation, c.mutationSchema.Mutation, query)
			return output
		})

		output := runMiddlewares(middlewares, &ComputationInput{
			Ctx:         ctx,
			Id:          id,
			ParsedQuery: query,
			Previous:    nil,
			Query:       mutate.Query,
			Variables:   mutate.Variables,
		})
		current, err := output.Current, output.Error

		c.logger.FinishExecution(ctx, tags, time.Since(start))

		if err != nil {
			c.writeOrClose(OutEnvelope{
				ID:       id,
				Type:     "error",
				Message:  sanitizeError(err),
				Metadata: output.Metadata,
			})

			go c.closeSubscription(id)

			if extractPathError(err) == context.Canceled {
				return nil, err
			}

			if _, ok := err.(SanitizedError); !ok {
				c.logger.Error(ctx, err, tags)
			}
			return nil, err
		}

		c.writeOrClose(OutEnvelope{
			ID:       id,
			Type:     "result",
			Message:  diff.Diff(nil, current),
			Metadata: output.Metadata,
		})

		go c.rerunSubscriptionsImmediately()

		return nil, errors.New("stop")
	}, MinRerunInterval)

	return nil
}

func (c *conn) rerunSubscriptionsImmediately() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, runner := range c.subscriptions {
		runner.RerunImmediately()
	}
}

func (c *conn) closeSubscription(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if runner, ok := c.subscriptions[id]; ok {
		runner.Stop()
		delete(c.subscriptions, id)
	}
}

func (c *conn) closeSubscriptions() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, runner := range c.subscriptions {
		runner.Stop()
		delete(c.subscriptions, id)
	}
}

type WebsocketWriter func(e OutEnvelope)
type WebsocketHandler func(e *InEnvelope, write WebsocketWriter) error

func (c *conn) handle(e *InEnvelope, write WebsocketWriter) error {
	switch e.Type {
	case "subscribe":
		var subscribe subscribeMessage
		if err := json.Unmarshal(e.Message, &subscribe); err != nil {
			return err
		}
		return c.handleSubscribe(e.ID, &subscribe)

	case "unsubscribe":
		c.closeSubscription(e.ID)
		return nil

	case "mutate":
		var mutate mutateMessage
		if err := json.Unmarshal(e.Message, &mutate); err != nil {
			return err
		}
		return c.handleMutate(e.ID, &mutate)

	case "echo":
		write(OutEnvelope{
			ID:       e.ID,
			Type:     "echo",
			Message:  nil,
			Metadata: nil,
		})
		return nil

	case "url":
		var url string
		if err := json.Unmarshal(e.Message, &url); err != nil {
			return err
		}
		c.url = url
		return nil

	default:
		return NewSafeError("unknown message type")
	}
}

type simpleLogger struct {
}

func (s *simpleLogger) StartExecution(ctx context.Context, tags map[string]string, initial bool) {
}
func (s *simpleLogger) FinishExecution(ctx context.Context, tags map[string]string, delay time.Duration) {
}
func (s *simpleLogger) Error(ctx context.Context, err error, tags map[string]string) {
	log.Printf("error:%v\n%s", tags, err)
}

func Handler(schema *Schema) http.Handler {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrader.Upgrade: %v", err)
			return
		}
		defer socket.Close()

		makeCtx := func(ctx context.Context) context.Context {
			return ctx
		}

		ServeJSONSocket(r.Context(), socket, schema, makeCtx, &simpleLogger{})
	})
}

func (c *conn) Use(fn MiddlewareFunc) {
	c.middlewares = append(c.middlewares, fn)
}

func ServeJSONSocket(ctx context.Context, socket JSONSocket, schema *Schema, makeCtx MakeCtxFunc, logger GraphqlLogger) {
	conn := CreateJSONSocket(ctx, socket, schema, makeCtx, logger)
	conn.ServeJSONSocket()
}

func CreateJSONSocket(ctx context.Context, socket JSONSocket, schema *Schema, makeCtx MakeCtxFunc, logger GraphqlLogger) *conn {
	return &conn{
		socket: socket,
		ctx:    ctx,

		schema:         schema,
		mutationSchema: schema,
		makeCtx:        makeCtx,
		logger:         logger,

		subscriptions: make(map[string]*reactive.Rerunner),
	}
}

func CreateJSONSocketWithMutationSchema(ctx context.Context, socket JSONSocket, schema, mutationSchema *Schema, makeCtx MakeCtxFunc, logger GraphqlLogger) *conn {
	return &conn{
		socket: socket,
		ctx:    ctx,

		schema:         schema,
		mutationSchema: mutationSchema,
		makeCtx:        makeCtx,
		logger:         logger,

		subscriptions: make(map[string]*reactive.Rerunner),
	}
}

func (c *conn) ServeJSONSocket(handlers ...WebsocketHandler) {
	defer c.closeSubscriptions()

	handlers = append(handlers, c.handle)

	for {
		var envelope InEnvelope
		if err := c.socket.ReadJSON(&envelope); err != nil {
			if !isCloseError(err) {
				log.Println("socket.ReadJSON:", err)
			}
			return
		}

		for _, handler := range handlers {
			if err := handler(&envelope, c.writeOrClose); err != nil {
				log.Println("c.handle:", err)
				c.writeOrClose(OutEnvelope{
					ID:       envelope.ID,
					Type:     "error",
					Message:  sanitizeError(err),
					Metadata: nil,
				})
			}
		}
	}
}
