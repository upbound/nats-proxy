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

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"

	"github.com/upbound/nats-proxy/pkg/natsproxy"
)

const oneKB = 1024
const defaultResponseSize = 3 * oneKB

/**
Usage:
Run nats locally: docker run -p 4222:4222 -ti nats:latest
Run this main()
In browser:
http://localhost:1323/
http://localhost:1323/?sleep=200&chunkSize=512&responseSize=10000

Outputs chunks as numbered blocks of output 1-9. i.e.
> http://localhost:1323/?sleep=0&chunkSize=5&responseSize=50
00000111112222233333444445555566666777778888899999
*/
func main() {
	opts := []nats.Option{nats.Name("test-nats-client")}
	opts = natsproxy.SetupConnOptions(opts)
	proxyReadTimeout := time.Second * 10
	agentKeepAliveInterval := time.Second * 5

	// Connect to NATS
	nc, err := nats.Connect(nats.DefaultURL, opts...)
	if err != nil {
		// TODO: just unavailable, not fatal, should reconnect.
		logrus.Fatal(err)
	}

	bs := backendServer()
	defer bs.Close()

	agentID := uuid.New()
	tr := TestRouter{
		backendURL: bs.URL,
		e:          echo.New(),
		nc:         nc,
		agentID:    agentID,
		np:         natsproxy.NewHTTPProxy(nc, proxyReadTimeout),
	}

	tr.e.Any("/*", tr.routeRequest())

	bp := backendProxy{}
	ag := natsproxy.NewAgent(nc, agentID, &bp, natsproxy.DefaultSubjectForAgentFunc(agentID.String()), agentKeepAliveInterval)

	err = ag.Listen()
	if err != nil {
		panic(err)
	}
	logrus.SetOutput(os.Stdout)
	logrus.SetLevel(logrus.DebugLevel)

	// defer agentSub.Unsubscribe()
	tr.e.Logger.Fatal(tr.e.Start(":1323"))
}

// TestRouter E2E test nats routing functionality
type TestRouter struct {
	backendURL string
	e          *echo.Echo
	nc         *nats.Conn
	np         *natsproxy.HTTPProxy
	agentID    uuid.UUID
}

func getIntFromQueryParam(u *url.URL, key string, defaultValue int) int {
	data := u.Query().Get(key)
	if data != "" {
		if v, err := strconv.Atoi(data); err == nil {
			defaultValue = v
		}
	}

	return defaultValue
}

func backendServer() *httptest.Server {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseSize := getIntFromQueryParam(r.URL, "responseSize", defaultResponseSize)
		chunkSize := getIntFromQueryParam(r.URL, "chunkSize", oneKB)
		sleep := time.Duration(getIntFromQueryParam(r.URL, "sleep", 1000))

		// Enforce responseSize being a multiple of chunksize
		responseSize -= responseSize % chunkSize

		logrus.Debugf("backend received request - chunkSize: %d, responseSize %d, sleep %d", chunkSize, responseSize, sleep*time.Millisecond)
		w.Header().Set("Content-length", fmt.Sprintf("%d", responseSize))

		// 1MB buffer that'll be copied multiple times to the response
		for i := 0; i < responseSize/chunkSize; i++ {
			buf := []byte(strings.Repeat(fmt.Sprintf("%d", i%10), chunkSize))
			if _, err := w.Write(buf); err != nil {
				logrus.Debug("backendServer - failed to write to response. Error: ", err.Error())
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if sleep > 0 {
				time.Sleep(sleep * time.Millisecond)
			}
		}
	}))
	return backendServer
}

func (r *TestRouter) routeRequest() echo.HandlerFunc {
	return func(c echo.Context) error {
		logrus.Debugf("TestRouter.routeRequest() %s\n", c.Request().URL.String())
		u, _ := url.Parse(r.backendURL)
		c.Request().URL.Host = u.Host   // Only overwrite host
		c.Request().URL.Scheme = "http" // Test server doesn't have scheme, but proxy requires it.
		r.np.ServeHTTP(c.Response(), c.Request(), natsproxy.DefaultSubjectForAgentFunc(r.agentID.String()))
		return nil
	}
}

type backendProxy struct{}

func (b *backendProxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	logrus.Debugf("backendProxy.ServeHttp() %s\n", r.URL.String())
	pxy := httputil.NewSingleHostReverseProxy(r.URL)
	pxy.FlushInterval = -1
	pxy.ServeHTTP(rw, r)
}
