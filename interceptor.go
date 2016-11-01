package interceptor

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/getlantern/errors"
	"github.com/getlantern/golog"
	"github.com/getlantern/hidden"
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
	Pipe(op ops.Op, w http.ResponseWriter, req *http.Request, defaultPort int)
	HTTP(op ops.Op, w http.ResponseWriter, req *http.Request, forwardInitialRequest bool, defaultPort int)
}

// Opts configures an Interceptor.
type Opts struct {
	Dial                func(req *http.Request, addr string, port int) (net.Conn, error)
	GetBuffer           func() []byte
	PutBuffer           func(buf []byte)
	OnRequest           func(req *http.Request) *http.Request
	OnInitialOK         func(resp *http.Response, req *http.Request) *http.Response
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

func (ic *interceptor) dialUpstream(op ops.Op, downstream io.Writer, req *http.Request, defaultPort int) net.Conn {
	addr := hostIncludingPort(req, defaultPort)
	port, err := portForAddress(addr)
	if err != nil {
		ic.respondBadGatewayHijacked(downstream, req, op.FailIf(errors.New("Unable to determine port for address %v: %v", addr, err)))
		return nil
	}

	upstream, err := ic.Dial(req, addr, port)
	if err != nil {
		ic.respondBadGatewayHijacked(downstream, req, op.FailIf(errors.New("Could not dial '%v': %v", addr, err)))
		return nil
	}

	return upstream
}

func (ic *interceptor) respondOK(writer io.Writer, req *http.Request, respHeaders http.Header) error {
	return ic.respondHijacked(writer, req, http.StatusOK, respHeaders, nil)
}

func (ic *interceptor) respondBadGatewayHijacked(writer io.Writer, req *http.Request, err error) error {
	log.Debugf("Responding %v", http.StatusBadGateway)
	var body []byte
	if err != nil {
		body = []byte(hidden.Clean(err.Error()))
	}
	return ic.respondHijacked(writer, req, http.StatusBadGateway, make(http.Header), body)
}

func (ic *interceptor) respondHijacked(writer io.Writer, req *http.Request, statusCode int, respHeaders http.Header, body []byte) error {
	defer func() {
		if req.Body != nil {
			if err := req.Body.Close(); err != nil {
				log.Debugf("Error closing body of request: %s", err)
			}
		}
	}()

	if respHeaders == nil {
		respHeaders = make(http.Header)
	}
	resp := &http.Response{
		Header:     respHeaders,
		StatusCode: statusCode,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	if body != nil {
		resp.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	if statusCode == http.StatusOK {
		resp = ic.OnInitialOK(resp, req)
	}
	return resp.Write(writer)
}

func respondBadGateway(w http.ResponseWriter, err error) {
	log.Debugf("Responding BadGateway: %v", err)
	w.WriteHeader(http.StatusBadGateway)
	if _, writeError := w.Write([]byte(hidden.Clean(err.Error()))); writeError != nil {
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
