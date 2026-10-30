package grpcwire

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// Handler serves one call on a stream.
type Handler func(s *ServerStream) error

// ServerStream is the server side of one call.
type ServerStream struct {
	ctx  context.Context
	r    *http.Request
	w    http.ResponseWriter
	mu   sync.Mutex
	sent bool
}

// Context is cancelled when the client goes away or the call's deadline passes.
func (s *ServerStream) Context() context.Context { return s.ctx }

// Metadata returns the first value of a request metadata key (an HTTP/2 header; keys are case-insensitive).
func (s *ServerStream) Metadata(key string) string { return s.r.Header.Get(key) }

// Recv reads the next request message; io.EOF when the client has finished sending.
func (s *ServerStream) Recv(m proto.Message) error { return readFrame(s.r.Body, m) }

// Send writes one response message and flushes it to the client.
func (s *ServerStream) Send(m proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.sent = true
	if err := writeFrame(s.w, m); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}

// Unary adapts a unary method.
func Unary[Req any, PReq interface {
	*Req
	proto.Message
}, Resp proto.Message](f func(context.Context, PReq) (Resp, error)) Handler {
	return func(s *ServerStream) error {
		req := PReq(new(Req))
		if err := s.Recv(req); err != nil {
			if errors.Is(err, io.EOF) {
				return Errorf(InvalidArgument, "no request message")
			}
			return err
		}
		resp, err := f(s.ctx, req)
		if err != nil {
			return err
		}
		return s.Send(resp)
	}
}

// Server routes gRPC calls by full method name ("/package.Service/Method").
type Server struct {
	mu       sync.Mutex
	methods  map[string]Handler
	srv      *http.Server
	shutdown bool
}

// NewServer returns an empty server.
func NewServer() *Server { return &Server{methods: map[string]Handler{}} }

// Handle registers h for a full method name.
func (s *Server) Handle(fullMethod string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[fullMethod] = h
}

// Methods lists the registered full method names.
func (s *Server) Methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.methods))
	for m := range s.methods {
		out = append(out, m)
	}
	return out
}

// ServeHTTP serves one HTTP/2 request as a gRPC call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		http.Error(w, "herd-guest serves gRPC only", http.StatusUnsupportedMediaType)
		return
	}
	if r.ProtoMajor != 2 {
		http.Error(w, "gRPC needs HTTP/2", http.StatusHTTPVersionNotSupported)
		return
	}
	w.Header().Set("Content-Type", "application/grpc+proto")
	s.mu.Lock()
	h := s.methods[r.URL.Path]
	s.mu.Unlock()

	ctx := r.Context()
	if t := r.Header.Get("Grpc-Timeout"); t != "" {
		if d, ok := parseTimeout(t); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}
	st := &ServerStream{ctx: ctx, r: r, w: w}
	var err error
	if h == nil {
		err = Errorf(Unimplemented, "unknown method %s", r.URL.Path)
	} else {
		err = h(st)
	}
	code, msg := statusOf(err)
	st.mu.Lock()
	defer st.mu.Unlock()
	w.Header().Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(int(code)))
	if msg != "" {
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", encodeMessage(msg))
	}
}

// Serve accepts connections on l and serves them with unencrypted HTTP/2 (prior knowledge, as gRPC clients dial
// with insecure credentials) until Close. It returns nil after Close.
func (s *Server) Serve(l net.Listener) error {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: s, Protocols: p, ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.srv = srv
	s.mu.Unlock()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops serving and cuts every open call.
func (s *Server) Close() error {
	s.mu.Lock()
	s.shutdown = true
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// parseTimeout parses a grpc-timeout value: up to 8 digits and a unit (H, M, S, m, u, n).
func parseTimeout(v string) (time.Duration, bool) {
	if len(v) < 2 || len(v) > 9 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[:len(v)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	unit := map[byte]time.Duration{'H': time.Hour, 'M': time.Minute, 'S': time.Second, 'm': time.Millisecond, 'u': time.Microsecond, 'n': time.Nanosecond}[v[len(v)-1]]
	if unit == 0 {
		return 0, false
	}
	return time.Duration(n) * unit, true
}
