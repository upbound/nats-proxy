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
	agent := NewAgent(nc, agentID, &TestHandler{result: resData}, DefaultSubjectForAgentFunc, time.Second*5)
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
