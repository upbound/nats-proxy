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
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"

	"github.com/upbound/nats-proxy/pkg/testhelper"
)

type TestHandler struct {
	result []byte
}

func (t TestHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	rw.Write(t.result)
}

func TestE2EFlow(t *testing.T) {
	s, port := testhelper.RunDefaultServer()
	defer s.Shutdown()
	nc := testhelper.NewConnection(t, port)
	defer nc.Close()

	g := NewGomegaWithT(t)

	agentID := uuid.New()
	reqData := []byte("baz")
	resData := []byte("zap")

	// 1. Listen on an agent.
	agent := NewAgent(nc, agentID, &TestHandler{result: resData}, DefaultSubjectForAgentFunc(agentID.String()), time.Second*5)
	err := agent.Listen()
	defer agent.Drain()
	g.Expect(err).To(BeNil())

	// 2. Send Request to agent
	timeout := time.Second * 1
	pxy := NewHTTPProxy(nc, timeout)
	req := httptest.NewRequest(http.MethodGet, "/foo", bytes.NewReader(reqData))
	res := httptest.NewRecorder()
	pxy.ServeHTTP(res, req, DefaultSubjectForAgentFunc(agentID.String()))

	g.Expect(res.Body.String()).To(Equal(string(resData)))
}
