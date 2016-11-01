package interceptor

import (
	"bufio"
	"bytes"
	"errors"
	"github.com/getlantern/httptest"
	"github.com/getlantern/mockconn"
	"github.com/getlantern/ops"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net"
	"net/http"
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
	i := New(&Opts{
		Dial: func(req *http.Request, addr string, port int) (net.Conn, error) {
			conn, err := d.Dial("tcp", addr)
			return conn, err
		},
	})
	req, _ := http.NewRequest("CONNECT", "http://thehost:123", nil)
	go i.HTTP(op, w, req, true, 756)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, "thehost:123", d.LastDialed(), "Should have used specified port of 123")
	body := w.Body().String()
	assert.Contains(t, body, "HTTP/1.1 502 Bad Gateway")
	assert.Contains(t, body, "Could not dial 'thehost:123': I don't want to dial")
}

func TestCONNECT(t *testing.T) {
	doTest(t, ops.Begin("TestCONNECT"), "CONNECT", true, true)
}

func TestPipe(t *testing.T) {
	doTest(t, ops.Begin("TestPipeDontForwardFirst"), "GET", true, false)
}

func TestHTTPForwardFirst(t *testing.T) {
	doTest(t, ops.Begin("TestHTTPForwardFirst"), "GET", false, true)
}

func TestHTTPDontForwardFirst(t *testing.T) {
	doTest(t, ops.Begin("TestHTTPDontForwardFirst"), "GET", false, false)
}

func doTest(t *testing.T, op ops.Op, requestMethod string, pipe bool, forwardInitialRequest bool) {
	defer op.End()
	nestedReqBody := []byte("My Request")
	nestedReq, _ := http.NewRequest("POST", "http://subdomain2.thehost/stuff", ioutil.NopCloser(bytes.NewBuffer(nestedReqBody)))
	nestedReq.Proto = "HTTP/1.1"
	nestedReqText := dumpRequest(nestedReq)

	nestedReq2Body := []byte("My Request")
	nestedReq2, _ := http.NewRequest("POST", "http://subdomain3.thehost/stuff", ioutil.NopCloser(bytes.NewBuffer(nestedReq2Body)))
	nestedReq2.Proto = "HTTP/1.0"
	nestedReq2Text := dumpRequest(nestedReq2)

	respBody := []byte("My Response")
	resp := httptest.NewRecorder(nil)
	resp.WriteHeader(http.StatusCreated)
	resp.Write(respBody)
	respText := dumpResponse(resp.Result())

	d := mockconn.SucceedingDialer([]byte(respText))
	w := httptest.NewRecorder(append([]byte(nestedReqText), nestedReq2Text...))
	i := New(&Opts{
		Dial: func(req *http.Request, addr string, port int) (net.Conn, error) {
			conn, err := d.Dial("tcp", addr)
			return conn, err
		},
		OnInitialOK: func(resp *http.Response, req *http.Request) *http.Response {
			log.Debug("Setting OK header")
			resp.Header.Set(okHeader, "I'm OK!")
			return resp
		},
		OnRequest: func(req *http.Request) *http.Request {
			if req.RemoteAddr == "" {
				t.Fatal("Request missing RemoteAddr!")
			}
			return req
		},
	})

	req, _ := http.NewRequest(requestMethod, "http://subdomain.thehost", nil)
	req.RemoteAddr = "remoteaddr:134"
	if pipe {
		i.Pipe(op, w, req, 756)
	} else {
		i.HTTP(op, w, req, forwardInitialRequest, 756)
	}

	if pipe {
		assert.Equal(t, "subdomain.thehost:756", d.LastDialed(), "Should have defaulted port to 756")
	} else {
		assert.Equal(t, "subdomain3.thehost:756", d.LastDialed(), "Should have defaulted port to 756")
	}

	r := bufio.NewReader(w.Body())
	isConnect := requestMethod == "CONNECT"
	if isConnect {
		resp, err := http.ReadResponse(r, nil)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "I'm OK!", resp.Header.Get(okHeader))
	}

	recvResp, err := http.ReadResponse(r, nil)
	if !assert.NoError(t, err) {
		return
	}
	recvRespBody, err := ioutil.ReadAll(recvResp.Body)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, string(respBody), string(recvRespBody))
	if !pipe {
		assert.NotNil(t, recvResp.Header.Get("Date"))
	}

	received := bufio.NewReader(bytes.NewBuffer(d.Received()))
	if !isConnect && forwardInitialRequest {
		recReq, err := http.ReadRequest(received)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, dumpRequest(req), dumpRequest(recReq), "Should have forwarded initial request")
	}

	if pipe {
		recvNestedReqText, err := ioutil.ReadAll(received)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, nestedReqText+nestedReq2Text, string(recvNestedReqText), "Piped request should be unchanged")
	} else {
		recNestedReq, err := http.ReadRequest(received)
		if !assert.NoError(t, err) {
			return
		}
		recNestedReqBody, err := ioutil.ReadAll(recNestedReq.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, string(nestedReqBody), string(recNestedReqBody), "Should have received piped data from downstream")
		assert.Equal(t, "HTTP/1.1", recNestedReq.Proto, "Protocol should have been upgraded to 1.1")
		assert.Equal(t, "/stuff", recNestedReq.URL.String(), "Host should have been stripped from URL")
		assert.Equal(t, "subdomain2.thehost", recNestedReq.Host, "Host should have been populated correctly")
	}

	assert.True(t, w.Closed(), "Downstream connection not closed")
	assert.True(t, d.AllClosed(), "Upstream connection not closed")
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
