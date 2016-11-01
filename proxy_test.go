package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"github.com/getlantern/fdcount"
	"github.com/getlantern/httptest"
	"github.com/getlantern/mockconn"
	"github.com/getlantern/ops"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

const (
	okHeader = "X-Test-OK"
)

func TestDialFailure(t *testing.T) {
	op := ops.Begin("TestDialFailure")
	defer op.End()
	d := mockconn.FailingDialer(errors.New("I don't want to dial"))
	w := httptest.NewRecorder(nil)
	h := HTTP(false, 0, nil, nil, d.Dial)
	req, _ := http.NewRequest("CONNECT", "http://thehost:123", nil)
	go h(op, w, req)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, "thehost:123", d.LastDialed(), "Should have used specified port of 123")
	body := w.Body().String()
	assert.Empty(t, body)
}

func TestCONNECT(t *testing.T) {
	doTest(t, ops.Begin("TestCONNECT"), "CONNECT", false)
}

func TestHTTPForwardFirst(t *testing.T) {
	doTest(t, ops.Begin("TestHTTPForwardFirst"), "GET", false)
}

func TestHTTPDontForwardFirst(t *testing.T) {
	doTest(t, ops.Begin("TestHTTPDontForwardFirst"), "GET", true)
}

func doTest(t *testing.T, op ops.Op, requestMethod string, discardFirstRequest bool) {
	defer op.End()

	l, err := net.Listen("tcp", "localhost:0")
	if !assert.NoError(t, err) {
		return
	}
	defer l.Close()

	pl, err := net.Listen("tcp", "localhost:0")
	if !assert.NoError(t, err) {
		return
	}
	defer l.Close()

	var mx sync.RWMutex
	seenAddresses := make(map[string]bool)
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mx.Lock()
		seenAddresses[req.RemoteAddr] = true
		mx.Unlock()
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(req.Host))
	}))

	dial := func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", l.Addr().String())
	}

	onRequest := func(req *http.Request) *http.Request {
		if req.RemoteAddr == "" {
			t.Fatal("Request missing RemoteAddr!")
		}
		return req
	}

	isConnect := requestMethod == "CONNECT"
	var h Handler
	if isConnect {
		h = CONNECT(756, 30*time.Second, nil, dial)
	} else {
		h = HTTP(discardFirstRequest, 30*time.Second, onRequest, nil, dial)
	}

	go http.Serve(pl, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h(op, w, req)
	}))

	_, counter, err := fdcount.Matching("TCP")
	if !assert.NoError(t, err) {
		return
	}

	// We use a single connection for all requests, even though they're going to
	// different hosts. This simulates user agents like Firefox and Edge that
	// send requests for multiple hosts across a single proxy connection.
	conn, err := net.Dial("tcp", pl.Addr().String())
	if !assert.NoError(t, err) {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	roundTrip := func(req *http.Request, readResponse bool) (*http.Response, string, error) {
		rtErr := req.Write(conn)
		if rtErr != nil {
			return nil, "", rtErr
		}
		if readResponse {
			resp, rtErr := http.ReadResponse(br, req)
			if rtErr != nil {
				return nil, "", rtErr
			}
			body, rtErr := ioutil.ReadAll(resp.Body)
			if rtErr != nil {
				return resp, "", rtErr
			}
			return resp, string(body), nil
		}
		return nil, "", nil
	}

	req, _ := http.NewRequest(requestMethod, "http://subdomain.thehost", nil)
	req.RemoteAddr = "remoteaddr:134"

	includeFirst := isConnect || !discardFirstRequest
	resp, body, err := roundTrip(req, includeFirst)
	if !assert.NoError(t, err) {
		return
	}
	if !isConnect && !discardFirstRequest {
		assert.Equal(t, "subdomain.thehost", body, "Should have left port alone")
		assert.Contains(t, resp.Header.Get("Keep-Alive"), "timeout", "First response's headers should contain a Keep-Alive timeout")
	}

	nestedReqBody := []byte("My Request")
	nestedReq, _ := http.NewRequest("POST", "http://subdomain2.thehost/a", ioutil.NopCloser(bytes.NewBuffer(nestedReqBody)))
	nestedReq.Proto = "HTTP/1.1"
	resp, body, err = roundTrip(nestedReq, true)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "subdomain2.thehost", body, "Should have gotten right host")
	if !isConnect {
		assert.Contains(t, resp.Header.Get("Keep-Alive"), "timeout", "All HTTP responses' headers should contain a Keep-Alive timeout")
	}

	nestedReq2Body := []byte("My Request")
	nestedReq2, _ := http.NewRequest("POST", "http://subdomain3.thehost/b", ioutil.NopCloser(bytes.NewBuffer(nestedReq2Body)))
	nestedReq2.Proto = "HTTP/1.0"
	resp, body, err = roundTrip(nestedReq2, true)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "subdomain3.thehost", body, "Should have gotten right host")
	if !isConnect {
		assert.Contains(t, resp.Header.Get("Keep-Alive"), "timeout", "All HTTP responses' headers should contain a Keep-Alive timeout")
	}

	expectedConnections := 3
	if discardFirstRequest {
		expectedConnections--
	}
	if isConnect {
		expectedConnections = 1
	}
	mx.RLock()
	defer mx.RUnlock()
	assert.Equal(t, expectedConnections, len(seenAddresses))

	conn.Close()
	assert.NoError(t, counter.AssertDelta(0), "All connections should have been closed")
}

func dumpRequest(req *http.Request) string {
	buf := &bytes.Buffer{}
	req.Write(buf)
	return buf.String()
}

func dumpResponse(resp *http.Response) string {
	buf := &bytes.Buffer{}
	resp.Write(buf)
	return buf.String()
}
