package qrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/oklog/run"
	"go.uber.org/ratelimit"
)

var (
	// ErrWriteAfterCloseSelf when try to write after closeself
	ErrWriteAfterCloseSelf = errors.New("write after closeself")
	// ErrRstNonExistingStream when reset non existing stream
	ErrRstNonExistingStream = errors.New("reset non existing stream")
)

// FrameWriter looks like writes a qrpc resp
// but it internally needs be scheduled, thus maintains a simple yet powerful interface
type FrameWriter interface {
	StartWrite(requestID uint64, cmd Cmd, flags FrameFlag)
	WriteBytes(v []byte) // v is copied in WriteBytes
	EndWrite() error     // block until scheduled

	ResetFrame(requestID uint64, reason Cmd) error
}

// A Handler responds to an qrpc request.
type Handler interface {
	ServeQRPC(FrameWriter, *RequestFrame)
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as qrpc handlers. If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler that calls f.
type HandlerFunc func(FrameWriter, *RequestFrame)

// ServeQRPC calls f(w, r).
func (f HandlerFunc) ServeQRPC(w FrameWriter, r *RequestFrame) {
	f(w, r)
}

// MiddlewareFunc will return false to abort
type MiddlewareFunc func(FrameWriter, *RequestFrame) bool

// ServeMux is qrpc request multiplexer.
type ServeMux struct {
	mu sync.RWMutex
	m  map[Cmd]Handler
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux { return new(ServeMux) }

// HandleFunc registers the handler function for the given pattern.
func (mux *ServeMux) HandleFunc(cmd Cmd, handler func(FrameWriter, *RequestFrame), middleware ...MiddlewareFunc) {
	mux.Handle(cmd, HandlerFunc(handler), middleware...)
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, handle panics.
func (mux *ServeMux) Handle(cmd Cmd, handler Handler, middleware ...MiddlewareFunc) {
	mux.mu.Lock()
	defer mux.mu.Unlock()

	if handler == nil {
		panic("qrpc: nil handler")
	}
	if _, exist := mux.m[cmd]; exist {
		panic("qrpc: multiple registrations for " + string(cmd))
	}

	if mux.m == nil {
		mux.m = make(map[Cmd]Handler)
	}
	mux.m[cmd] = HandlerWithMW(handler, middleware...)
}

// ServeQRPC dispatches the request to the handler whose
// cmd matches the request.
func (mux *ServeMux) ServeQRPC(w FrameWriter, r *RequestFrame) {
	mux.mu.RLock()
	h, ok := mux.m[r.Cmd]
	if !ok {
		LogError("cmd not registered", r.Cmd)
		r.Close()
		return
	}
	mux.mu.RUnlock()
	h.ServeQRPC(w, r)
}

// Server defines parameters for running an qrpc server.
type Server struct {
	// one handler for each listening address
	bindings []ServerBinding
	upTime   time.Time

	// manages below
	mu           sync.Mutex
	listeners    map[net.Listener]struct{}
	doneChan     chan struct{}
	shutdownFunc []func()
	done         bool

	id2Conn          []sync.Map
	activeConn       []sync.Map // for better iterate when write, map[*serveconn]struct{}
	throttle         []atomic.Value
	closeRateLimiter []ratelimit.Limiter

	wg sync.WaitGroup // wait group for goroutines

	pushID uint64
}

type throttle struct {
	on bool
	ch chan struct{}
}

// NewServer creates a server
func NewServer(bindings []ServerBinding) *Server {
	closeRateLimiter := make([]ratelimit.Limiter, len(bindings))
	for idx, binding := range bindings {
		if binding.MaxCloseRate != 0 {
			closeRateLimiter[idx] = ratelimit.New(binding.MaxCloseRate)
		}
	}
	return &Server{
		bindings:         bindings,
		upTime:           time.Now(),
		listeners:        make(map[net.Listener]struct{}),
		doneChan:         make(chan struct{}),
		id2Conn:          make([]sync.Map, len(bindings)),
		activeConn:       make([]sync.Map, len(bindings)),
		throttle:         make([]atomic.Value, len(bindings)),
		closeRateLimiter: closeRateLimiter,
	}
}

// ListenAndServe starts listening on all bindings
func (srv *Server) ListenAndServe() (err error) {

	err = srv.ListenAll()
	if err != nil {
		return
	}
	return srv.ServeAll()
}

// ListenAll for listen on all bindings
func (srv *Server) ListenAll() (err error) {

	for i, binding := range srv.bindings {

		var ln net.Listener

		if binding.ListenFunc != nil {
			ln, err = binding.ListenFunc("tcp", binding.Addr)
		} else {
			ln, err = net.Listen("tcp", binding.Addr)
		}
		if err != nil {
			return
		}

		if binding.OverlayNetwork != nil {
			srv.bindings[i].ln = binding.OverlayNetwork(ln)
		} else {
			srv.bindings[i].ln = ln.(*net.TCPListener)
		}
	}

	return
}

// ServeAll for serve on all bindings
func (srv *Server) ServeAll() error {
	var g run.Group

	for i := range srv.bindings {
		idx := i
		binding := srv.bindings[i]
		g.Add(func() error {
			return srv.Serve(binding.ln, idx)
		}, func(err error) {
			serr := srv.Shutdown()
			LogError("err", err, "serr", serr)
		})
	}

	return g.Run()
}

// Listener defines required listener methods for qrpc
type Listener interface {
	net.Listener
	SetDeadline(t time.Time) error
}

var (

	// ErrServerClosed is returned by the Server's Serve, ListenAndServe,
	// methods after a call to Shutdown or Close.
	ErrServerClosed = errors.New("qrpc: Server closed")
	// ErrListenerAcceptReturnType when Listener.Accept doesn't return TCPConn
	ErrListenerAcceptReturnType = errors.New("qrpc: Listener.Accept doesn't return TCPConn")
	// ErrAcceptTimedout when accept timed out
	ErrAcceptTimedout = errors.New("qrpc: accept timed out")

	defaultAcceptTimeout = 5 * time.Second
)

// Serve accepts incoming connections on the Listener qrpcListener, creating a
// new service goroutine for each. The service goroutines read requests and
// then call srv.Handler to reply to them.
//
// Serve always returns a non-nil error. After Shutdown or Close, the
// returned error is ErrServerClosed.
func (srv *Server) Serve(qrpcListener Listener, idx int) error {

	l := tcpKeepAliveListener{qrpcListener}
	defer l.Close()
	var tempDelay time.Duration // how long to sleep on accept failure

	srv.trackListener(l, true)
	defer srv.trackListener(l, false)

	serveCtx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	for {
		srv.waitThrottle(idx, srv.doneChan)
		l.SetDeadline(time.Now().Add(defaultAcceptTimeout))
		rw, e := l.Accept()
		if e != nil {
			select {
			case <-srv.doneChan:
				return ErrServerClosed
			default:
			}
			if e == ErrAcceptTimedout {
				// for overlay network
				continue
			}
			if opError, ok := e.(*net.OpError); ok && opError.Timeout() {
				// don't log the scheduled timeout
				continue
			}
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				LogError("qrpc: Accept error", e, "retrying in", tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			LogError("qrpc: Accept fatal error", e) // accept4: too many open files in system
			time.Sleep(time.Second)                 // keep trying instead of quit
			continue
		}
		tempDelay = 0

		GoFunc(&srv.wg, func() {
			c := srv.newConn(serveCtx, rw, idx)
			c.serve()
		})
	}
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections.
type tcpKeepAliveListener struct {
	Listener
}

// TCPConn in qrpc's aspect
type TCPConn interface {
	net.Conn
	SetKeepAlive(keepalive bool) error
	SetKeepAlivePeriod(d time.Duration) error
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	c, err = ln.Listener.Accept()
	if err != nil {
		return
	}

	var (
		tc TCPConn
		ok bool
	)
	if tc, ok = c.(TCPConn); !ok {
		err = ErrListenerAcceptReturnType
		return
	}

	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(20 * time.Second)

	return
}

func (srv *Server) trackListener(ln net.Listener, add bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if add {
		srv.listeners[ln] = struct{}{}
	} else {
		delete(srv.listeners, ln)
	}
}

// Create new connection from rwc.
func (srv *Server) newConn(ctx context.Context, rwc net.Conn, idx int) (sc *serveconn) {
	if srv.bindings[idx].ReadFrameChSize > 0 {
		sc = &serveconn{
			server:       srv,
			rwc:          rwc,
			idx:          idx,
			untrackedCh:  make(chan struct{}),
			cs:           &ConnStreams{},
			readFrameCh:  make(chan readFrameResult, srv.bindings[idx].ReadFrameChSize),
			writeFrameCh: make(chan writeFrameRequest)}
	} else {
		sc = &serveconn{
			server:       srv,
			rwc:          rwc,
			idx:          idx,
			untrackedCh:  make(chan struct{}),
			cs:           &ConnStreams{},
			readFrameCh:  make(chan readFrameResult),
			writeFrameCh: make(chan writeFrameRequest)}
	}

	ctx, cancelCtx := context.WithCancel(ctx)
	ctx = context.WithValue(ctx, ConnectionInfoKey, &ConnectionInfo{SC: sc})

	sc.cancelCtx = cancelCtx
	sc.ctx = ctx

	srv.activeConn[idx].Store(sc, struct{}{})

	return sc
}

var kickOrder uint64

// bindID bind the id to sc
// it is concurrent safe
func (srv *Server) bindID(sc *serveconn, id string) (kick bool, ko uint64) {

	idx := sc.idx

check:
	v, loaded := srv.id2Conn[idx].LoadOrStore(id, sc)

	if loaded {
		vsc := v.(*serveconn)
		if vsc == sc {
			return
		}
		ok, ch := srv.untrack(vsc, true)
		if !ok {
			<-ch
		}
		LogDebug(unsafe.Pointer(sc), "trigger closeUntracked", unsafe.Pointer(vsc))

		err := vsc.closeUntracked()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok {
				err = opErr.Err
			}
		}

		if srv.bindings[idx].CounterMetric != nil {
			errStr := fmt.Sprintf("%v", err)
			countlvs := []string{"method", "kickoff", "error", errStr}
			srv.bindings[idx].CounterMetric.With(countlvs...).Add(1)
		}

		atomic.AddUint64(&kickOrder, 1)
		kick = true

		goto check
	}

	ko = atomic.LoadUint64(&kickOrder)
	return
}

func (srv *Server) untrack(sc *serveconn, kicked bool) (bool, <-chan struct{}) {

	locked := atomic.CompareAndSwapUint32(&sc.untrack, 0, 1)
	if !locked {
		return false, sc.untrackedCh
	}
	idx := sc.idx

	id := sc.GetID()
	if id != "" {
		srv.id2Conn[idx].Delete(id)
	}
	srv.activeConn[idx].Delete(sc)

	if kicked {
		if srv.bindings[idx].OnKickCB != nil {
			srv.bindings[idx].OnKickCB(sc.GetWriter())
		}
	}
	close(sc.untrackedCh)
	return true, sc.untrackedCh
}

var shutdownPollInterval = 500 * time.Millisecond

// Shutdown gracefully shutdown the server
func (srv *Server) Shutdown() error {

	srv.mu.Lock()
	if srv.done {
		srv.mu.Unlock()
		goto done
	}

	{
		lnerr := srv.closeListenersLocked()
		if lnerr != nil {
			srv.mu.Unlock()
			return lnerr
		}
	}

	srv.done = true
	srv.mu.Unlock()

	close(srv.doneChan)

	for _, f := range srv.shutdownFunc {
		f()
	}

done:
	srv.wg.Wait()

	return nil
}

// OnShutdown registers f to be called when shutdown
func (srv *Server) OnShutdown(f func()) {

	srv.mu.Lock()
	if srv.done {
		srv.mu.Unlock()
		f()
	}

	srv.shutdownFunc = append(srv.shutdownFunc, f)
	srv.mu.Unlock()

}

// GetPushID gets the pushId
func (srv *Server) GetPushID() uint64 {
	pushID := atomic.AddUint64(&srv.pushID, 1)
	return pushID
}

// WalkConnByID iterates over  serveconn by ids
func (srv *Server) WalkConnByID(idx int, ids []string, f func(FrameWriter, *ConnectionInfo)) {
	for _, id := range ids {
		v, ok := srv.id2Conn[idx].Load(id)
		if ok {
			sc := v.(*serveconn)
			f(v.(*serveconn).GetWriter(), sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo))
		}
	}
}

// GetConnectionInfoByID returns the ConnectionInfo for idx+id
func (srv *Server) GetConnectionInfoByID(idx int, id string) *ConnectionInfo {
	v, ok := srv.id2Conn[idx].Load(id)
	if !ok {
		return nil
	}

	return v.(*serveconn).ctx.Value(ConnectionInfoKey).(*ConnectionInfo)
}

// WalkConn walks through each serveconn
func (srv *Server) WalkConn(idx int, f func(FrameWriter, *ConnectionInfo) bool) {
	srv.activeConn[idx].Range(func(k, v interface{}) bool {
		sc := k.(*serveconn)
		return f(sc.GetWriter(), sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo))
	})
}

func (srv *Server) closeListenersLocked() (err error) {
	for ln := range srv.listeners {
		if err = ln.Close(); err != nil {
			return
		}
		delete(srv.listeners, ln)
	}
	return
}

// waitThrottle is concurrent safe
func (srv *Server) waitThrottle(idx int, doneCh <-chan struct{}) {
	v := srv.throttle[idx].Load()
	t, ok := v.(throttle)
	if ok && t.on {
		select {
		case <-t.ch:
		case <-doneCh:
		}
	}
}

// SetThrottle sets throttle on
func (srv *Server) SetThrottle(idx int) {
	v := srv.throttle[idx].Load()
	if v != nil {
		// already on,do nothing
		if v.(throttle).on {
			return
		}
	}
	srv.throttle[idx].Store(throttle{on: true, ch: make(chan struct{})})
}

// ClearThrottle clears throttle onff
func (srv *Server) ClearThrottle(idx int) {
	v := srv.throttle[idx].Load()
	if v == nil {
		return
	}
	close(v.(throttle).ch)

	srv.throttle[idx].Store(throttle{on: false, ch: make(chan struct{})})
}
