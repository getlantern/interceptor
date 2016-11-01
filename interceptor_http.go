package interceptor

import (
	"bufio"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/ops"
)

type up struct {
	conn      net.Conn
	requests  chan *http.Request
	responses chan *http.Response
}

func (ic *interceptor) HTTP(op ops.Op, w http.ResponseWriter, req *http.Request, forwardInitialRequest bool, defaultPort int) {
	var downstream net.Conn
	var downstreamBuffered *bufio.ReadWriter
	upstreams := make(map[string]*up)
	var err error

	closeDownstream := false
	defer func() {
		if closeDownstream {
			if closeErr := downstream.Close(); closeErr != nil {
				log.Tracef("Error closing downstream connection: %s", closeErr)
			}
		}
		for _, upstream := range upstreams {
			if closeErr := upstream.conn.Close(); closeErr != nil {
				log.Tracef("Error closing upstream connection: %s", closeErr)
			}
		}
	}()

	// Hijack underlying connection.
	downstream, downstreamBuffered, err = w.(http.Hijacker).Hijack()
	if err != nil {
		respondBadGateway(w, op.FailIf(errors.New("Unable to hijack connection: %s", err)))
		return
	}
	closeDownstream = true

	nextResponses := make(chan chan *http.Response, 100)
	ops.Go(func() {
		ic.processRequests(op, defaultPort, req.RemoteAddr, forwardInitialRequest, req, downstream, downstreamBuffered, upstreams, nextResponses)
	})
	ic.writeResponses(op, downstream, nextResponses)
	return
}

func (ic *interceptor) processRequests(op ops.Op, defaultPort int, remoteAddr string, forwardInitialRequest bool, req *http.Request, downstream net.Conn, downstreamBuffered *bufio.ReadWriter, upstreams map[string]*up, nextResponses chan chan *http.Response) {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(nextResponses)
	}()

	var readErr error
	first := true

	for {
		addr := hostIncludingPort(req, defaultPort)
		upstream := upstreams[addr]
		if upstream == nil {
			conn := ic.dialUpstream(op, downstream, req, defaultPort)
			if conn == nil {
				// Unable to dial, continue (dialUpstream will have sent a bad gateway already)
				if len(upstreams) == 0 {
					// No other connections ongoing, return
					return
				}
			}
			// Positive buffer size allows queuing multiple requests before getting a
			// response from the server.
			upstream = &up{conn, make(chan *http.Request, 100), make(chan *http.Response, 100)}
			defer close(upstream.requests)
			upstreams[addr] = upstream
			wg.Add(1)
			ops.Go(func() { ic.readResponses(op, &wg, downstream, downstreamBuffered, upstream) })
		}

		var out io.Writer = upstream.conn
		discardFirstRequest := first && !forwardInitialRequest
		if discardFirstRequest {
			// Discard first request
			out = ioutil.Discard
		} else {
			nextResponses <- upstream.responses
		}
		req = ic.OnRequest(req)
		req = prepareRequest(req)
		writeErr := req.Write(out)
		if writeErr != nil {
			if isUnexpected(writeErr) {
				log.Debug(op.FailIf(errors.New("Unable to write request to upstream: %v", writeErr)))
			}
			break
		}
		if !discardFirstRequest {
			upstream.requests <- req
		}

		req, readErr = http.ReadRequest(downstreamBuffered.Reader)
		if readErr != nil {
			if isUnexpected(readErr) {
				log.Debug(op.FailIf(errors.New("Unable to read next request from downstream: %v", readErr)))
				ic.OnReadRequestError(downstream, readErr)
			}
			break
		}

		// Preserve remote address from original request
		req.RemoteAddr = remoteAddr
		first = false
	}
}

func (ic *interceptor) readResponses(op ops.Op, wg *sync.WaitGroup, downstream net.Conn, downstreamBuffered *bufio.ReadWriter, upstream *up) {
	defer func() {
		close(upstream.responses)
		wg.Done()
	}()

	originBuffered := bufio.NewReader(upstream.conn)
	for req := range upstream.requests {
		resp, readErr := http.ReadResponse(originBuffered, req)
		if readErr != nil {
			if isUnexpected(readErr) {
				log.Debug(op.FailIf(errors.New("Unable to read next response from upstream: %v", readErr)))
				ic.OnReadResponseError(downstream, req, readErr)
			}
			return
		}
		upstream.responses <- resp
	}
}

func (ic *interceptor) writeResponses(op ops.Op, downstream net.Conn, nextResponses chan chan *http.Response) {
	responseNumber := 0
	for nextResponse := range nextResponses {
		resp, ok := <-nextResponse
		if !ok {
			// Done reading responses
			return
		}
		var out io.Writer = downstream
		belowHTTP11 := !resp.Request.ProtoAtLeast(1, 1)
		if belowHTTP11 && resp.StatusCode < 200 {
			// HTTP 1.0 doesn't define status codes below 200, discard response
			// see http://coad.measurement-factory.com/cgi-bin/coad/SpecCgi?session_id=57f811c0_8182_4a442469&spec_id=rfc2616#excerpt/rfc2616/859a092cb26bde76c25284196171c94d
			out = ioutil.Discard
		} else {
			resp = ic.OnResponse(prepareResponse(resp, belowHTTP11), resp.Request, responseNumber)
			responseNumber++
		}
		writeErr := resp.Write(out)
		if writeErr != nil {
			if isUnexpected(writeErr) {
				log.Debug(op.FailIf(errors.New("Unable to write response to downstream: %v", writeErr)))
			}
			break
		}
		if resp.Close {
			// Server wants us to close connection, do that
			return
		}
	}
}

// prepareRequest prepares the request in line with the HTTP spec for proxies.
func prepareRequest(req *http.Request) *http.Request {
	outReq := new(http.Request)
	// Beware, this will make a shallow copy. We have to copy all maps
	*outReq = *req

	outReq.Proto = "HTTP/1.1"
	outReq.ProtoMajor = 1
	outReq.ProtoMinor = 1
	// Overwrite close flag: keep persistent connection for the backend servers
	outReq.Close = false

	// Request Header
	outReq.Header = make(http.Header)
	copyHeadersForForwarding(outReq.Header, req.Header)
	// Ensure we have a HOST header (important for Go 1.6+ because http.Server
	// strips the HOST header from the inbound request)
	outReq.Header.Set("Host", req.Host)

	// Request URL
	outReq.URL = cloneURL(req.URL)
	// We know that is going to be HTTP always because HTTPS isn't forwarded.
	// We need to hardcode it here because req.URL.Scheme can be undefined, since
	// client request don't need to use absolute URIs
	outReq.URL.Scheme = "http"
	// We need to make sure the host is defined in the URL (not the actual URI)
	outReq.URL.Host = req.Host
	outReq.URL.RawQuery = req.URL.RawQuery
	outReq.Body = req.Body

	userAgent := req.UserAgent()
	if userAgent == "" {
		outReq.Header.Del("User-Agent")
	} else {
		outReq.Header.Set("User-Agent", userAgent)
	}

	return outReq
}

// prepareResponse prepares the response in line with the HTTP spec
func prepareResponse(resp *http.Response, belowHTTP11 bool) *http.Response {
	origHeader := resp.Header
	resp.Header = make(http.Header)
	copyHeadersForForwarding(resp.Header, origHeader)
	// Below added due to CoAdvisor test failure
	if resp.Header.Get("Date") == "" {
		resp.Header.Set("Date", time.Now().Format(time.RFC850))
	}
	if belowHTTP11 {
		// Also, make sure we're not sending chunked transfer encoding to 1.0 clients
		resp.TransferEncoding = nil
	}
	return resp
}

// cloneURL provides update safe copy by avoiding shallow copying User field
func cloneURL(i *url.URL) *url.URL {
	out := *i
	if i.User != nil {
		out.User = &(*i.User)
	}
	return &out
}

// copyHeadersForForwarding will copy the headers but filter those that shouldn't be
// forwarded
func copyHeadersForForwarding(dst, src http.Header) {
	var extraHopByHopHeaders []string
	for k, vv := range src {
		switch k {
		// Skip hop-by-hop headers, ref section 13.5.1 of http://www.ietf.org/rfc/rfc2616.txt
		case "Connection":
			// section 14.10 of rfc2616
			// the slice is short typically, don't bother sort it to speed up lookup
			extraHopByHopHeaders = vv
		case "Keep-Alive":
		case "Proxy-Authenticate":
		case "Proxy-Authorization":
		case "TE":
		case "Trailers":
		case "Transfer-Encoding":
		case "Upgrade":
		default:
			if !contains(k, extraHopByHopHeaders) {
				for _, v := range vv {
					dst.Add(k, v)
				}
			}
		}
	}
}

func contains(k string, s []string) bool {
	for _, h := range s {
		if k == h {
			return true
		}
	}
	return false
}

func (ic *interceptor) applyHTTPDefaults() {
	if ic.OnRequest == nil {
		ic.OnRequest = ic.defaultOnRequest
	}
	if ic.OnResponse == nil {
		ic.OnResponse = ic.defaultOnResponse
	}
	if ic.OnResponse == nil {
		ic.OnResponse = ic.defaultOnResponse
	}
	if ic.OnReadRequestError == nil {
		ic.OnReadRequestError = ic.defaultOnReadRequestError
	}
	if ic.OnReadResponseError == nil {
		ic.OnReadResponseError = ic.defaultOnReadResponseError
	}
}

func (ic *interceptor) defaultOnRequest(req *http.Request) *http.Request {
	return req
}

func (ic *interceptor) defaultOnResponse(resp *http.Response, req *http.Request, responseNumber int) *http.Response {
	return resp
}

func (ic *interceptor) defaultOnReadRequestError(w io.Writer, readErr error) {
	// do nothing
}

func (ic *interceptor) defaultOnReadResponseError(w io.Writer, req *http.Request, readErr error) {
	// do nothing
}
