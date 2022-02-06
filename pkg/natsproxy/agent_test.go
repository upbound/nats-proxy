// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package natsproxy

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/upbound/nats-proxy/pkg/testhelper"
)

type mockResponseSender struct {
	responses    []*Response
	triggerError error
}

func (m *mockResponseSender) send(response *Response) error {
	if m.triggerError != nil {
		return m.triggerError
	}
	m.responses = append(m.responses, response)
	return nil
}

func TestResponseWriterHeader(t *testing.T) {
	mock := &mockResponseSender{responses: []*Response{}}
	rw := NewResponseWriter(mock)

	rw.Header().Set("Foo", "baz")
	rw.WriteHeader(http.StatusCreated)

	g := NewGomegaWithT(t)
	g.Expect(mock.responses[0].Header["Foo"].Arr[0]).To(Equal("baz"))
	g.Expect(int(mock.responses[0].StatusCode)).To(Equal(http.StatusCreated))
}

func TestResponseWrite(t *testing.T) {
	mock := &mockResponseSender{responses: []*Response{}}
	rw := NewResponseWriter(mock)
	g := NewGomegaWithT(t)

	defaultResponseCode := http.StatusOK
	fakeBody := []byte("baz")

	ln, err := rw.Write(fakeBody)
	g.Expect(ln).To(Equal(len(fakeBody)))
	g.Expect(err).To(BeNil())

	// Headers and code form implicit writeHeaders with write.
	g.Expect(int(mock.responses[0].StatusCode)).To(Equal(defaultResponseCode))

	// Actual response from write.
	g.Expect(mock.responses[1].Body).To(Equal(fakeBody))
}

func TestResponseWriteWithErr(t *testing.T) {
	sendErr := errors.New("boom")
	mock := &mockResponseSender{triggerError: sendErr, responses: []*Response{}}
	rw := NewResponseWriter(mock)
	g := NewGomegaWithT(t)

	fakeBody := []byte("baz")

	ln, err := rw.Write(fakeBody)
	g.Expect(ln).To(Equal(0))
	g.Expect(err).To(Equal(sendErr))
	g.Expect(len(mock.responses)).To(Equal(0))
}

type publishData struct {
	subject string
	data    []byte
}

type mockNatsPublisher struct {
	history    []publishData
	publishErr error
}

func (m *mockNatsPublisher) Publish(subj string, data []byte) error {
	if m.publishErr != nil {
		return m.publishErr
	}
	m.history = append(m.history, publishData{subject: subj, data: data})
	return nil
}

func TestResponseTransportSend(t *testing.T) {
	subject := "foosubject"
	nc := &mockNatsPublisher{history: []publishData{}}
	rt := responseTransport{
		nc:      nc,
		mux:     sync.Mutex{},
		subject: subject,
	}

	fakeBody := []byte("fakebody")
	res := &Response{Body: fakeBody}
	err := rt.send(res)

	g := NewGomegaWithT(t)
	g.Expect(err).To(BeNil())

	sentResponse := &Response{}
	err = proto.Unmarshal(nc.history[0].data, sentResponse)
	g.Expect(err).To(BeNil())

	g.Expect(sentResponse.Body).To(Equal(fakeBody))
	g.Expect(sentResponse.TransportInfo.Sequence).To(Equal(int32(0)))
}

func TestResponseTransportSendErr(t *testing.T) {
	subject := "foosubject"
	sendErr := errors.New("boom")
	nc := &mockNatsPublisher{history: []publishData{}, publishErr: sendErr}
	rt := responseTransport{
		nc:      nc,
		mux:     sync.Mutex{},
		subject: subject,
	}

	res := &Response{Body: []byte("fakebody")}
	err := rt.send(res)

	g := NewGomegaWithT(t)
	g.Expect(err).To(Equal(sendErr))
	g.Expect(len(nc.history)).To(Equal(0))
}

func TestResponseTransportKeepAlives(t *testing.T) {
	subject := "foosubject"
	nc := &mockNatsPublisher{history: []publishData{}}
	rt := responseTransport{
		nc:      nc,
		mux:     sync.Mutex{},
		subject: subject,
	}

	keepAliveInterval := time.Millisecond * 10
	rt.startKeepAlive(keepAliveInterval)
	// 10ms per keepalive + buffer to verify at least 8 keepalives below.
	<-time.After(time.Millisecond * 120)

	g := NewGomegaWithT(t)
	rt.stopKeepAlive()

	for seq, res := range nc.history {
		sentResponse := &Response{}
		err := proto.Unmarshal(res.data, sentResponse)
		g.Expect(err).To(BeNil())
		g.Expect(sentResponse.TransportInfo.Sequence).To(Equal(int32(seq)))
	}

	g.Expect(len(nc.history) >= 8).To(BeTrue())
}

func TestServeNewRequestExpectClose(t *testing.T) {
	s, port := testhelper.RunDefaultServer()
	defer s.Shutdown()
	nc := testhelper.NewConnection(t, port)
	defer nc.Close()
	g := NewGomegaWithT(t)
	done := make(chan bool)

	agentID := uuid.New()

	inbox := "random_inbox"
	th := TestHandler{result: []byte("baz")}
	a := NewAgent(nc, agentID, th, DefaultSubjectForAgentFunc(agentID.String()), time.Second)

	pxyHandler := func(m *nats.Msg) {
		response := Response{}
		err := proto.Unmarshal(m.Data, &response)
		g.Expect(err).To(BeNil())

		// Expect to see closing on end of response.
		if response.TransportInfo.Closing {
			done <- true
		}
	}

	nc.Subscribe(inbox, pxyHandler)
	req := &Request{
		Header:        make(map[string]*Values),
		TransportInfo: &TransportInfo{Sequence: 0},
	}
	a.transports.init(inbox)
	go a.serveNewRequest(inbox, req)

	// Assert closing was called.
	err := testhelper.WaitTime(done, time.Millisecond*100)
	g.Expect(err).To(BeNil())
}

type NeverResponderHandler struct {
	done chan bool
}

func (t NeverResponderHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	<-req.Context().Done() // Cancelled
	t.done <- true
}

func TestServeNewRequestCancelFromProxy(t *testing.T) {
	s, port := testhelper.RunDefaultServer()
	defer s.Shutdown()
	nc := testhelper.NewConnection(t, port)
	defer nc.Close()
	g := NewGomegaWithT(t)

	agentID := uuid.New()

	inbox := "random_inbox"
	nr := NeverResponderHandler{done: make(chan bool)}
	a := NewAgent(nc, agentID, nr, DefaultSubjectForAgentFunc(agentID.String()), time.Second)

	reqs := []*Request{
		{
			Header:        make(map[string]*Values),
			TransportInfo: &TransportInfo{Sequence: 0},
		},
		{
			Header:        make(map[string]*Values),
			TransportInfo: &TransportInfo{Sequence: 1, Closing: true},
		},
	}

	for _, req := range reqs {
		out, err := proto.Marshal(req)
		g.Expect(err).To(BeNil())
		msg := &nats.Msg{
			Subject: DefaultSubjectForAgentFunc(a.id.String()),
			Data:    out,
			Reply:   inbox,
		}
		a.recv(msg)
	}

	err := testhelper.WaitTime(nr.done, time.Second)
	g.Expect(err).To(BeNil())
}
