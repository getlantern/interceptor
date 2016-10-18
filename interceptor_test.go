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
)

func TestDialFailure(t *testing.T) {
	op := ops.Begin("TestDialFailure")
	defer op.End()
	d := mockconn.FailingDialer(errors.New("I don't want to dial"))
	w := httptest.NewRecorder(nil)
	i := New(&Opts{
		Dial: func(req *http.Request, addr string, port int) (net.Conn, bool, error) {
			conn, err := d.Dial("tcp", addr)
			return conn, false, err
		},
	})
	req, _ := http.NewRequest("CONNECT", "http://thehost:123", nil)
	i.Intercept(w, req, false, op, 756)
	assert.Equal(t, "thehost:123", d.LastDialed(), "Should have used specified port of 123")
	assert.Equal(t, http.StatusBadGateway, w.Code())
	assert.Equal(t, "Could not dial 'thehost:123': I don't want to dial", w.Body().String())
}

func TestCONNECT(t *testing.T) {
	doTest(t, ops.Begin("TestCONNECT"), "CONNECT", true, true)
}

func TestPipeKeepFirst(t *testing.T) {
	doTest(t, ops.Begin("TestPipeKeepFirst"), "GET", true, true)
}

func TestPipeDontKeepFirst(t *testing.T) {
	doTest(t, ops.Begin("TestPipeDontKeepFirst"), "GET", true, false)
}

// func TestHTTPKeepFirst(t *testing.T) {
// 	doTest(t, ops.Begin("TestPipeKeepFirst"), "GET", true, true)
// }

func doTest(t *testing.T, op ops.Op, requestMethod string, pipe bool, forwardInitialRequest bool) {
	defer op.End()
	nestedReq, _ := http.NewRequest("POST", "http://subdomain.thehost/stuff", ioutil.NopCloser(bytes.NewBuffer([]byte("My Request"))))
	nestedReqText := dumpRequest(nestedReq)
	resp := httptest.NewRecorder(nil)
	resp.WriteHeader(http.StatusCreated)
	resp.Write([]byte("My Response"))
	respText := dumpResponse(resp.Result())
	d := mockconn.SucceedingDialer([]byte(respText))
	w := httptest.NewRecorder([]byte(nestedReqText))
	i := New(&Opts{
		Dial: func(req *http.Request, addr string, port int) (net.Conn, bool, error) {
			log.Debug(addr)
			conn, err := d.Dial("tcp", addr)
			return conn, pipe, err
		},
	})
	req, _ := http.NewRequest(requestMethod, "http://thehost", nil)
	i.Intercept(w, req, forwardInitialRequest, op, 756)
	assert.Equal(t, "thehost:756", d.LastDialed(), "Should have defaulted port to 756")
	r := bufio.NewReader(w.Body())
	isConnect := requestMethod == "CONNECT"
	if isConnect {
		resp, err := http.ReadResponse(r, nil)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
	responseData, err := ioutil.ReadAll(r)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, string(respText), string(responseData))
	received := bufio.NewReader(bytes.NewBuffer(d.Received()))
	if !isConnect && forwardInitialRequest {
		recReq, err := http.ReadRequest(received)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, dumpRequest(req), dumpRequest(recReq), "Should have forwarded initial request")
	}
	remainingReceived, _ := ioutil.ReadAll(received)
	assert.Equal(t, nestedReqText, string(remainingReceived), "Should have received piped data from downstream")
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
