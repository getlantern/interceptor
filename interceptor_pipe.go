package interceptor

import (
	"io"
	"net"
	"net/http"

	"github.com/getlantern/errors"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/netx"
	"github.com/getlantern/ops"
)

func (ic *interceptor) Pipe(op ops.Op, w http.ResponseWriter, req *http.Request, defaultPort int, dial func(network, addr string) (net.Conn, error)) {
	var downstream net.Conn
	var upstream net.Conn
	var err error

	closeDownstream := false
	closeUpstream := false
	defer func() {
		if closeDownstream {
			if closeErr := downstream.Close(); closeErr != nil {
				log.Tracef("Error closing downstream connection: %s", closeErr)
			}
		}
		if closeUpstream {
			if closeErr := upstream.Close(); closeErr != nil {
				log.Tracef("Error closing upstream connection: %s", closeErr)
			}
		}
	}()

	// Hijack underlying connection.
	downstream, _, err = w.(http.Hijacker).Hijack()
	if err != nil {
		respondBadGateway(w, op.FailIf(errors.New("Unable to hijack connection: %s", err)))
		return
	}
	closeDownstream = true

	upstream, err = dial("tcp", hostIncludingPort(req, defaultPort))
	if err != nil {
		ic.respondBadGatewayHijacked(downstream, req, err)
		return
	}
	closeUpstream = true

	success := make(chan bool, 1)
	op.Go(func() {
		// For CONNECT requests, send OK response
		if req.Method == "CONNECT" {
			err := ic.respondOK(downstream, req, w.Header())
			if err != nil {
				op.FailIf(log.Errorf("Unable to respond OK: %s", err))
				success <- false
				return
			}
		}
		success <- true
	})

	if <-success {
		// Pipe data between the client and the proxy.
		bufOut := ic.GetBuffer()
		bufIn := ic.GetBuffer()
		defer ic.PutBuffer(bufOut)
		defer ic.PutBuffer(bufIn)
		writeErr, readErr := netx.BidiCopy(upstream, downstream, bufOut, bufIn)
		// Note - we ignore idled errors because these are okay per the HTTP spec.
		// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html#sec8.1.4
		if readErr != nil && readErr != io.EOF {
			log.Debugf("Error piping data to downstream: %v", readErr)
		} else if writeErr != nil && writeErr != idletiming.ErrIdled {
			log.Debugf("Error piping data to upstream: %v", writeErr)
		}
	}
}

func (ic *interceptor) applyPipeDefaults() {
	if ic.GetBuffer == nil {
		ic.GetBuffer = ic.defaultGetBuffer
	}
	if ic.PutBuffer == nil {
		ic.PutBuffer = ic.defaultPutBuffer
	}
	if ic.OnInitialOK == nil {
		ic.OnInitialOK = ic.defaultOnInitialOK
	}
}

func (ic *interceptor) defaultGetBuffer() []byte {
	// We use the same default buffer size as io.Copy.
	return make([]byte, 32768)
}

func (ic *interceptor) defaultPutBuffer(buf []byte) {
	// do nothing
}

func (ic *interceptor) defaultOnInitialOK(resp *http.Response, req *http.Request) *http.Response {
	// Do nothing
	return resp
}
