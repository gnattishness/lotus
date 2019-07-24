package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"

	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log"
	"golang.org/x/xerrors"
)

var log = logging.Logger("rpc")

var (
	errorType   = reflect.TypeOf(new(error)).Elem()
	contextType = reflect.TypeOf(new(context.Context)).Elem()
)

// ErrClient is an error which occurred on the client side the library
type ErrClient struct {
	err error
}

func (e *ErrClient) Error() string {
	return fmt.Sprintf("RPC client error: %s", e.err)
}

// Unwrap unwraps the actual error
func (e *ErrClient) Unwrap(err error) error {
	return e.err
}

type clientResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	ID      int64           `json:"id"`
	Error   *respError      `json:"error,omitempty"`
}

type makeChanSink func() (context.Context, func([]byte, bool))

type clientRequest struct {
	req   request
	ready chan clientResponse

	// retCh provides a context and sink for handling incoming channel messages
	retCh makeChanSink
}

// ClientCloser is used to close Client from further use
type ClientCloser func()

// NewClient creates new josnrpc 2.0 client
//
// handler must be pointer to a struct with function fields
// Returned value closes the client connection
// TODO: Example
func NewClient(addr string, namespace string, handler interface{}, requestHeader http.Header) (ClientCloser, error) {
	return NewMergeClient(addr, namespace, []interface{}{handler}, requestHeader)
}

type client struct {
	namespace string

	requests chan clientRequest
	idCtr int64
}

// NewMergeClient is like NewClient, but allows to specify multiple structs
// to be filled in the same namespace, using one connection
func NewMergeClient(addr string, namespace string, outs []interface{}, requestHeader http.Header) (ClientCloser, error) {
	conn, _, err := websocket.DefaultDialer.Dial(addr, requestHeader)
	if err != nil {
		return nil, err
	}

	c := client{
		namespace: namespace,
	}

	stop := make(chan struct{})
	c.requests = make(chan clientRequest)

	handlers := map[string]rpcHandler{}
	go (&wsConn{
		conn:     conn,
		handler:  handlers,
		requests: c.requests,
		stop:     stop,
	}).handleWsConn(context.TODO())

	for _, handler := range outs {
		htyp := reflect.TypeOf(handler)
		if htyp.Kind() != reflect.Ptr {
			return nil, xerrors.New("expected handler to be a pointer")
		}
		typ := htyp.Elem()
		if typ.Kind() != reflect.Struct {
			return nil, xerrors.New("handler should be a struct")
		}

		val := reflect.ValueOf(handler)

		for i := 0; i < typ.NumField(); i++ {
			fn, err := c.makeRpcFunc(typ.Field(i))
			if err != nil {
				return nil, err
			}

			val.Elem().Field(i).Set(fn)
		}
	}

	return func() {
		close(stop)
	}, nil
}

func (c *client) makeOutChan(ctx context.Context, ftyp reflect.Type, valOut int) (func() reflect.Value, makeChanSink) {
	retVal := reflect.Zero(ftyp.Out(valOut))

	chCtor := func() (context.Context, func([]byte, bool)) {
		// unpack chan type to make sure it's reflect.BothDir
		ctyp := reflect.ChanOf(reflect.BothDir, ftyp.Out(valOut).Elem())
		ch := reflect.MakeChan(ctyp, 0) // todo: buffer?
		retVal = ch.Convert(ftyp.Out(valOut))

		return ctx, func(result []byte, ok bool) {
			if !ok {
				// remote channel closed, close ours too
				ch.Close()
				return
			}

			val := reflect.New(ftyp.Out(valOut).Elem())
			if err := json.Unmarshal(result, val.Interface()); err != nil {
				log.Errorf("error unmarshaling chan response: %s", err)
				return
			}

			ch.Send(val.Elem()) // todo: select on ctx is probably a good idea
			}
	}

	return func() reflect.Value { return retVal }, chCtor
}

func (c *client) sendRequest(ctx context.Context, req request, chCtor makeChanSink) clientResponse {
	rchan := make(chan clientResponse, 1)
	c.requests <- clientRequest{
		req:   req,
		ready: rchan,

		retCh: chCtor,
	}
	var ctxDone <-chan struct{}
	var resp clientResponse

	if ctx != nil {
		ctxDone = ctx.Done()
	}

	// wait for response, handle context cancellation
loop:
	for {
		select {
		case resp = <-rchan:
			break loop
		case <-ctxDone: // send cancel request
			ctxDone = nil

			c.requests <- clientRequest{
				req: request{
					Jsonrpc: "2.0",
					Method:  wsCancel,
					Params:  []param{{v: reflect.ValueOf(*req.ID)}},
				},
			}
		}
	}

	return resp
}

type rpcFunc struct {
	client *client

	ftyp reflect.Type
	name string

	nout int
	valOut int
	errOut int

	hasCtx int
	retCh bool
}

func (fn *rpcFunc) processResponse(resp clientResponse, rval reflect.Value) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = rval
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		if resp.Error != nil {
			out[fn.errOut].Set(reflect.ValueOf(resp.Error))
		}
	}

	return out
}

func (fn *rpcFunc) processError(err error) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = reflect.New(fn.ftyp.Out(fn.valOut)).Elem()
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		out[fn.errOut].Set(reflect.ValueOf(&ErrClient{err}))
	}

	return out
}

func (fn *rpcFunc) handleRpcCall(args []reflect.Value) (results []reflect.Value) {
	id := atomic.AddInt64(&fn.client.idCtr, 1)
	params := make([]param, len(args)-fn.hasCtx)
	for i, arg := range args[fn.hasCtx:] {
		params[i] = param{
			v: arg,
		}
	}

	var ctx context.Context
	if fn.hasCtx == 1 {
		ctx = args[0].Interface().(context.Context)
	}

	retVal := func() reflect.Value { return reflect.Value{} }

	// if the function returns a channel, we need to provide a sink for the
	// messages
	var chCtor makeChanSink
	if fn.retCh {
		retVal, chCtor = fn.client.makeOutChan(ctx, fn.ftyp, fn.valOut)
	}

	req := request{
		Jsonrpc: "2.0",
		ID:      &id,
		Method:  fn.client.namespace + "." + fn.name,
		Params:  params,
	}

	resp := fn.client.sendRequest(ctx, req, chCtor)

	if resp.ID != *req.ID {
		return fn.processError(xerrors.New("request and response id didn't match"))
	}

	if fn.valOut != -1 && !fn.retCh {
		val := reflect.New(fn.ftyp.Out(fn.valOut))

		if resp.Result != nil {
			log.Debugw("rpc result", "type", fn.ftyp.Out(fn.valOut))
			if err := json.Unmarshal(resp.Result, val.Interface()); err != nil {
				return fn.processError(xerrors.Errorf("unmarshaling result: %w", err))
			}
		}

		retVal = func() reflect.Value { return val.Elem() }
	}

	return fn.processResponse(resp, retVal())
}

func (c *client) makeRpcFunc(f reflect.StructField) (reflect.Value, error) {
	ftyp := f.Type
	if ftyp.Kind() != reflect.Func {
		return reflect.Value{}, xerrors.New("handler field not a func")
	}

	fun := &rpcFunc{
		client: c,
		ftyp: ftyp,
		name: f.Name,
	}
	fun.valOut, fun.errOut, fun.nout = processFuncOut(ftyp)

	if ftyp.NumIn() > 0 && ftyp.In(0) == contextType {
		fun.hasCtx = 1
	}
	fun.retCh = fun.valOut != -1 && ftyp.Out(fun.valOut).Kind() == reflect.Chan

	return reflect.MakeFunc(ftyp, fun.handleRpcCall), nil
}