package interceptor

import (
	"bufio"
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
	op := ops.Begin("TestPipe")
	defer op.End()
	d := mockconn.SucceedingDialer([]byte("Response"))
	w := httptest.NewRecorder([]byte("Request"))
	i := New(&Opts{
		Dial: func(req *http.Request, addr string, port int) (net.Conn, bool, error) {
			log.Debug(addr)
			conn, err := d.Dial("tcp", addr)
			return conn, true, err
		},
	})
	req, _ := http.NewRequest("CONNECT", "http://thehost", nil)
	i.Intercept(w, req, false, op, 756)
	assert.Equal(t, "thehost:756", d.LastDialed(), "Should have defaulted port to 756")
	r := bufio.NewReader(w.Body())
	resp, err := http.ReadResponse(r, nil)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	responseData, err := ioutil.ReadAll(r)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "Response", string(responseData))
	assert.Equal(t, "Request", string(d.Received()))
	assert.True(t, w.Closed(), "Downstream connection not closed")
	assert.True(t, d.AllClosed(), "Upstream connection not closed")
}
