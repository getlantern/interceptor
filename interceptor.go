package interceptor

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/getlantern/errors"
	"github.com/getlantern/golog"
	"github.com/getlantern/ops"
)

var (
	log = golog.LoggerFor("interceptor")
)

type interceptor struct {
	Opts
}

// Interceptor is something that can intercept HTTP requests.
type Interceptor interface {
	Intercept(w http.ResponseWriter, req *http.Request, forwardInitialRequest bool, op ops.Op, defaultPort int)
}

// Opts configures an Interceptor.
type Opts struct {
	Dial                func(initialReq *http.Request, addr string, port int) (conn net.Conn, pipe bool, err error)
	GetBuffer           func() []byte
	PutBuffer           func(buf []byte)
	OnRequest           func(req *http.Request) *http.Request
	OnResponse          func(resp *http.Response, req *http.Request, responseNumber int) *http.Response
	OnReadRequestError  func(w io.Writer, readErr error)
	OnReadResponseError func(w io.Writer, req *http.Request, readErr error)
}

// New creates a new interceptor function using the given options.
func New(opts *Opts) Interceptor {
	ic := &interceptor{*opts}
	ic.applyHTTPDefaults()
	ic.applyPipeDefaults()
	return ic
}

func (ic *interceptor) Intercept(w http.ResponseWriter, req *http.Request, forwardInitialRequest bool, op ops.Op, defaultPort int) {
	addr := hostIncludingPort(req, defaultPort)
	port, err := portForAddress(addr)
	if err != nil {
		respondBadGateway(w, op.FailIf(errors.New("Unable to determine port for address %v: %v", addr, err)))
		return
	}

	var downstream net.Conn
	var downstreamBuffered *bufio.ReadWriter
	var upstream net.Conn

	defer func() {
		if downstream != nil {
			if closeErr := downstream.Close(); closeErr != nil {
				log.Tracef("Error closing downstream connection: %s", closeErr)
			}
		}
		if upstream != nil {
			if closeErr := upstream.Close(); closeErr != nil {
				log.Tracef("Error closing upstream connection: %s", closeErr)
			}
		}
	}()

	upstream, pipe, err := ic.Dial(req, addr, port)
	if err != nil {
		respondBadGateway(w, op.FailIf(errors.New("Could not dial %v", err)))
		return
	}

	// Hijack underlying connection.
	downstream, downstreamBuffered, err = w.(http.Hijacker).Hijack()
	if err != nil {
		respondBadGateway(w, op.FailIf(errors.New("Unable to hijack connection: %s", err)))
		return
	}

	if pipe {
		ic.pipe(req, forwardInitialRequest, op, downstream, downstreamBuffered, upstream)
	} else {
		ic.http(req, forwardInitialRequest, op, downstream, downstreamBuffered, upstream)
	}
}

func respondOK(writer io.Writer, req *http.Request) error {
	return respondHijacked(writer, req, http.StatusOK)
}

func respondBadGatewayHijacked(writer io.Writer, req *http.Request) error {
	log.Debugf("Responding %v", http.StatusBadGateway)
	return respondHijacked(writer, req, http.StatusBadGateway)
}

func respondHijacked(writer io.Writer, req *http.Request, statusCode int) error {
	defer func() {
		if err := req.Body.Close(); err != nil {
			log.Debugf("Error closing body of OK response: %s", err)
		}
	}()

	resp := &http.Response{
		StatusCode: statusCode,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	return resp.Write(writer)
}

func respondBadGateway(w http.ResponseWriter, err error) {
	log.Debugf("Responding BadGateway: %v", err)
	w.WriteHeader(http.StatusBadGateway)
	if _, writeError := w.Write([]byte(err.Error())); writeError != nil {
		log.Debugf("Error writing error to ResponseWriter: %v", writeError)
	}
}

// hostIncludingPort extracts the host:port from a request.  It fills in a
// a default port if none was found in the request.
func hostIncludingPort(req *http.Request, defaultPort int) string {
	_, port, err := net.SplitHostPort(req.Host)
	if port == "" || err != nil {
		return req.Host + ":" + strconv.Itoa(defaultPort)
	}
	return req.Host
}

func portForAddress(addr string) (int, error) {
	_, portString, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("Unable to determine port for address %v: %v", addr, err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		return 0, fmt.Errorf("Unable to parse port %v for address %v: %v", addr, port, err)
	}
	return port, nil
}

func isUnexpected(err error) bool {
	text := err.Error()
	return !strings.HasSuffix(text, "EOF") && !strings.Contains(text, "use of closed network connection") && !strings.Contains(text, "Use of idled network connection")
}
