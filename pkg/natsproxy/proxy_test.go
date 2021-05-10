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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/upbound/nats-proxy/pkg/testhelper"
)

func TestAgentResponseBroker(t *testing.T) {
	contentHeader := map[string][]string{
		"Content-Type": {"foo"},
	}

	type args struct {
		response []*Response
	}
	type want struct {
		code    int
		body    string
		headers http.Header
	}

	cases := map[string]struct {
		args
		want
	}{
		"MultiPartResponseWithHeader": {
			args: args{
				response: []*Response{
					{StatusCode: int32(http.StatusUnsupportedMediaType), Header: SerializeMap(contentHeader)},
					{Body: []byte("bazbody1")},
					{Body: []byte("bazbody2")},
					{TransportInfo: &TransportInfo{Closing: true}},
				},
			},
			want: want{
				code:    http.StatusUnsupportedMediaType,
				body:    "bazbody1bazbody2",
				headers: contentHeader,
			},
		},
		"CloseWithContentLen0": {
			args: args{
				response: []*Response{
					{
						Header: SerializeMap(map[string][]string{"Content-Length": {"0"}}),
					},
				},
			},
			want: want{
				code: http.StatusOK,
				body: "",
			},
		},
		"CloseWithContentLen5": {
			args: args{
				response: []*Response{
					{
						Header: SerializeMap(map[string][]string{"Content-Length": {"5"}}),
						Body:   []byte("12345"),
					},
				},
			},
			want: want{
				code: http.StatusOK,
				body: "12345",
			},
		},
	}

	for name, tc := range cases {
		rec := httptest.NewRecorder()
		cr := NewAgentResponseBroker(rec)
		handle := cr.handleResponse

		t.Run(name, func(t *testing.T) {
			go func() {
				for seq, res := range tc.args.response {
					res.TransportInfo = &TransportInfo{Sequence: int32(seq)}
					handle(res)
				}
				cr.done <- true
			}()

			g := NewGomegaWithT(t)
			err := testhelper.WaitTime(cr.done, time.Second)
			g.Expect(err).To(BeNil())

			res := rec.Result()
			defer res.Body.Close()

			for header, v := range tc.want.headers {
				g.Expect(res.Header.Get(header)).To(Equal(v[0]))
			}

			g.Expect(rec.Body.String()).To(Equal(tc.want.body))
			g.Expect(res.StatusCode).To(Equal(tc.want.code))
		})
	}
}

func TestTunnelTimeout(t *testing.T) {
	s, port := testhelper.RunDefaultServer()
	defer s.Shutdown()
	nc := testhelper.NewConnection(t, port)
	defer nc.Close()

	timeout := time.Millisecond * 50

	type args struct {
		messageFrequency time.Duration
		responses        []*Response
	}
	type want struct {
		timeout      bool
		messageCount int
	}

	cases := map[string]struct {
		args
		want
	}{
		"KeepAliveUntilClose": {
			args: args{
				messageFrequency: timeout - time.Millisecond*25,
				responses: []*Response{
					{TransportInfo: &TransportInfo{KeepAlive: true}},
					{TransportInfo: &TransportInfo{KeepAlive: true}},
					{TransportInfo: &TransportInfo{Closing: true}},
				},
			},
			want: want{
				messageCount: 0,
				timeout:      false,
			},
		},
		"TimeoutWithNoKeepAlive": {
			args: args{},
			want: want{
				messageCount: 0,
				timeout:      true,
			},
		},
		"NoTimeoutWithResponses": {
			args: args{
				messageFrequency: timeout - time.Millisecond*25,
				responses: []*Response{
					{Body: []byte("foo"), TransportInfo: &TransportInfo{}},
					{Body: []byte("baz"), TransportInfo: &TransportInfo{}},
					{Body: []byte("biz"), TransportInfo: &TransportInfo{}},
					{TransportInfo: &TransportInfo{Closing: true}},
				},
			},
			want: want{
				messageCount: 3,
				timeout:      false,
			},
		},
		"TimeoutWithSlowResponse": {
			args: args{
				messageFrequency: timeout + (time.Millisecond * 75),
				responses: []*Response{
					{Body: []byte("foo"), TransportInfo: &TransportInfo{}},
				},
			},
			want: want{
				messageCount: 0,
				timeout:      true,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			messageCount := 0
			tun := newTunnel(nc, "foo", timeout, func(res *Response) {
				messageCount++
				logrus.Print("received message")
			})

			err := tun.open()
			if err != nil {
				t.Errorf("unexpected err %s", err.Error())
			}
			defer tun.close()

			go func() {
				for seq, res := range tc.args.responses {
					logrus.Print("send message")
					time.Sleep(tc.messageFrequency)

					res.TransportInfo.Sequence = int32(seq)
					out, err := proto.Marshal(res)
					g.Expect(err).To(BeNil())
					msg := &nats.Msg{
						Subject: tun.reply,
						Data:    out,
					}

					err = nc.PublishMsg(msg)
					g.Expect(err).To(BeNil())
				}
			}()

			if tc.timeout {
				<-tun.mon.TimedOut()
			} else {
				<-tun.done
			}

			g.Expect(tc.want.messageCount).To(Equal(messageCount))
		})
	}
}
