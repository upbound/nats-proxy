package natsproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// NewAgent will connect to nats in main cluster and proxy connections locally
// to an http.Handler.
// TODO: add reasonable defaults for keepAliveInterval
func NewAgent(nc *nats.Conn, id uuid.UUID, handler http.Handler, subject string, keepAliveInterval time.Duration) *Agent {
	return &Agent{
		nc:          nc,
		id:          id,
		handler:     handler,
		natsSubject: subject,
		transports: transportManager{
			nc:                nc,
			mux:               sync.Mutex{},
			transports:        map[string]*responseTransport{},
			keepAliveInterval: keepAliveInterval,
		},
	}
}

// Agent listens for incoming proxy requests for a given agent.
type Agent struct {
	nc           *nats.Conn
	id           uuid.UUID
	handler      http.Handler
	transports   transportManager
	subscription *nats.Subscription
	natsSubject  string
}

// Listen initiates nats queue subscription for the agent.
func (a *Agent) Listen() error {
	if a.subscription == nil {
		sub, err := a.nc.QueueSubscribe(a.natsSubject, emptyQ, a.recv)
		if err != nil {
			return err
		}
		a.subscription = sub
	}

	return nil
}

// Drain de-registers interest in all subscriptions and drains
// connections to gracefully shut down.
func (a *Agent) Drain() error {
	return a.nc.Drain()
}

// IsDraining verifies the state of Drain on the nats connection
func (a *Agent) IsDraining() bool {
	return a.nc.IsDraining()
}

func (a *Agent) serveNewRequest(reply string, request *Request) {
	rt := a.transports.get(reply)
	defer a.transports.close(reply)

	rw := NewResponseWriter(rt)
	req, err := unmarshalHTTPRequest(rt.request.ctx, request)
	if err != nil {
		http.Error(rw, fmt.Sprintf("unexpected marshal err %s", err), http.StatusInternalServerError)
		return
	}

	lh := req.Header.Clone()
	lh.Set("Authorization", "[REDACTED]")
	lh.Set("Cookie", "[REDACTED]")
	logrus.Debugf("Agent.serveNewRequest request for %s, host: %s, headers: %+v", req.URL.String(), req.URL.Host, lh)
	a.handler.ServeHTTP(rw, req)
}

// recv translates requests from  nats <=> http
func (a *Agent) recv(msg *nats.Msg) {
	request := &Request{}
	err := proto.Unmarshal(msg.Data, request)
	if err != nil {
		logrus.WithError(err).Error("Agent.recv() - message not unserializable")
		return
	}

	if request.TransportInfo.Sequence == 0 {
		a.transports.init(msg.Reply)
		go a.serveNewRequest(msg.Reply, request)
	} else {
		// TODO: rework this tunnel all requests into a transport recv instead
		// of this in between if / else based on sequence.
		rt := a.transports.get(msg.Reply)
		if rt != nil && request.TransportInfo.Closing {
			rt.close(true)
		}
	}
}

type transportManager struct {
	nc                *nats.Conn
	mux               sync.Mutex
	transports        map[string]*responseTransport
	keepAliveInterval time.Duration
}

func (a *transportManager) init(reply string) *responseTransport {
	a.mux.Lock()
	defer a.mux.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	rc := &requestContext{ctx: ctx, cancel: cancel}
	rt := &responseTransport{
		nc:      a.nc,
		mux:     sync.Mutex{},
		subject: reply,
		request: rc,
	}
	rt.startKeepAlive(a.keepAliveInterval)
	a.transports[reply] = rt

	return rt
}

func (a *transportManager) get(reply string) *responseTransport {
	a.mux.Lock()
	defer a.mux.Unlock()
	return a.transports[reply]
}

func (a *transportManager) close(reply string) {
	rt := a.get(reply)
	if rt != nil {
		rt.stopKeepAlive()
		rt.close(false)
		a.mux.Lock()
		delete(a.transports, reply)
		a.mux.Unlock()
	}
}

// responseSender interface used to ease testing
type responseSender interface {
	send(response *Response) error
}

// natsPublisher interface used to ease testing.
type natsPublisher interface {
	Publish(subj string, data []byte) error
}

type requestContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type responseTransport struct {
	nc        natsPublisher
	mux       sync.Mutex
	subject   string
	sequence  int32
	keepAlive *keepAlive
	closed    bool
	request   *requestContext
}

func (r *responseTransport) startKeepAlive(interval time.Duration) {
	if r.keepAlive == nil {
		r.keepAlive = &keepAlive{
			interval: interval,
			tunnel:   r,
		}
	}

	r.keepAlive.reset()
}

func (r *responseTransport) stopKeepAlive() {
	if r.keepAlive != nil {
		r.keepAlive.stop()
		r.keepAlive = nil
	}
}

func (r *responseTransport) send(response *Response) error {
	if response.TransportInfo == nil {
		response.TransportInfo = &TransportInfo{}
	}

	if r.closed && !response.TransportInfo.Closing {
		return io.ErrClosedPipe
	}

	if !r.closed && r.keepAlive != nil {
		r.keepAlive.reset()
	}

	r.mux.Lock()
	response.TransportInfo.Sequence = r.sequence
	r.sequence++
	out, err := proto.Marshal(response)
	if err != nil {
		r.mux.Unlock()
		logrus.Error("responseTransport.send - unable to marshal response")
		return err
	}
	err = r.nc.Publish(r.subject, out)
	if err != nil {
		r.mux.Unlock()
		logrus.Error("responseTransport.send - unable to send response")
		return err
	}

	r.mux.Unlock()
	return nil
}

// checkAndSetClosing returns if closed was already set.
func (r *responseTransport) checkAndSetClosed() bool {
	logrus.Debug("responseTransport.checkAndSetClosed")
	r.mux.Lock()
	defer r.mux.Unlock()
	closedBefore := r.closed
	r.closed = true
	return closedBefore
}

func (r *responseTransport) close(fromProxy bool) {
	logrus.Debug("responseTransport.close")
	if r.checkAndSetClosed() {
		return
	}
	logrus.Debug("responseTransport.stopKeepAlives")
	r.stopKeepAlive()
	r.request.cancel()

	if !fromProxy {
		res := &Response{TransportInfo: &TransportInfo{Closing: true}}
		err := r.send(res)
		if err != nil {
			logrus.WithError(err).Error("responseTransport.close")
		}
	}
}

// NewResponseWriter returns an instance of ResponseWriter
func NewResponseWriter(transport responseSender) http.ResponseWriter {
	return &ResponseWriter{
		headers:   http.Header{},
		transport: transport,
	}
}

// ResponseWriter serializes and writes http responses over nats as part of Agent.
type ResponseWriter struct {
	transport responseSender
	Committed bool
	Status    int
	headers   http.Header
}

// Header returns the header map of the Response
func (rw *ResponseWriter) Header() http.Header {
	logrus.Debug("ResponseWriter.Header")
	return rw.headers
}

// WriteHeader writes the http code and headers to output.
func (rw *ResponseWriter) WriteHeader(code int) {
	logrus.Debug("ResponseWriter.WriteHeader")
	if rw.Committed {
		logrus.Warn("ResponseWriter.WriteHeader - response already committed")
		return
	}

	res := &Response{StatusCode: int32(code), Header: SerializeMap(rw.headers)}
	err := rw.transport.send(res)
	// TODO: we should only publish in write because it can return an error.
	// maybe buffer code only, and set a timer.

	if err != nil {
		logrus.WithError(err).Error("ResponseWriter.WriteHeader")
	}

	rw.Status = code
	rw.Committed = true
}

// Write will write bytes to body of the response. If headers have not been written
// this will write headers.
func (rw *ResponseWriter) Write(p []byte) (n int, err error) {
	logrus.Debug("ResponseWriter.Write()")
	if !rw.Committed {
		rw.WriteHeader(http.StatusOK)
	}

	res := &Response{Body: p}
	err = rw.transport.send(res)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

// Flush does nothing, but conforms to flusher interface to maintain interoperable behavior with the
// previous proxy that is still in use.
func (rw *ResponseWriter) Flush() {
	// TODO: we could easily enable flush, but it's assumed that
	// nats has sane buffering, so we'll test with this as a noop
	// while we have both the legacy http proxy enabled beside nats.
	logrus.Debug("ResponseWriter.Flush() - NOOP - TODO: disable flush when migration done")
}

type keepAlive struct {
	timer    *time.Timer
	interval time.Duration
	tunnel   responseSender
}

func (k *keepAlive) send() {
	logrus.Debug("keepAlive.send()")
	k.reset()
	res := &Response{TransportInfo: &TransportInfo{KeepAlive: true}}
	err := k.tunnel.send(res)
	if err != nil {
		logrus.WithError(err).Error("keepAlive.send()")
	}
}

func (k *keepAlive) reset() {
	logrus.Debug("keepAlive.reset()")
	if k.timer != nil {
		k.stop()
	}

	k.timer = time.AfterFunc(k.interval, k.send)
}

func (k *keepAlive) stop() {
	logrus.Debug("keepAlive.stopTimer()")
	if !k.timer.Stop() {
		select {
		case <-k.timer.C:
		default:
		}
	}
}
