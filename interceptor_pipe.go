package interceptor

import (
	"bufio"
	"io"
	"net"
	"net/http"

	"github.com/getlantern/idletiming"
	"github.com/getlantern/netx"
	"github.com/getlantern/ops"
)

func (ic *interceptor) pipe(req *http.Request, forwardInitialRequest bool, op ops.Op, downstream net.Conn, downstreamBuffered *bufio.ReadWriter, upstream net.Conn) {
	success := make(chan bool, 1)
	op.Go(func() {
		// For CONNECT requests, send OK response
		if req.Method == "CONNECT" {
			err := respondOK(downstream, req)
			if err != nil {
				op.FailIf(log.Errorf("Unable to respond OK: %s", err))
				success <- false
				return
			}
		} else if forwardInitialRequest {
			err := req.Write(upstream)
			if err != nil {
				op.FailIf(log.Errorf("Unable to write initial request: %v", err))
				respondBadGatewayHijacked(downstream, req)
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
}

func (ic *interceptor) defaultGetBuffer() []byte {
	return make([]byte, 32768)
}

func (ic *interceptor) defaultPutBuffer(buf []byte) {
	// do nothing
}
