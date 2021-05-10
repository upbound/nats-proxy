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
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// InboxPrefix is the prefix for all inbox subjects.
const (
	emptyQ         = ""
	replySuffixLen = 8 // Gives us 62^8
	rdigits        = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	base           = 62
)

// SubjectForAgentFunc is function to obtains NATS subject for a given agent ID
type SubjectForAgentFunc func(agentID string) string

// DefaultSubjectForAgentFunc returns the default NATS subject for a given agent
func DefaultSubjectForAgentFunc(agentID string) string {
	return fmt.Sprintf("agent.%s", agentID)
}

// SerializeMap converts golang map to protobuf map.
func SerializeMap(m map[string][]string) map[string]*Values {
	if m == nil {
		return nil
	}
	result := map[string]*Values{}
	for k, v := range m {
		cpy := make([]string, len(v))
		copy(cpy, v)
		result[k] = &Values{
			Arr: cpy,
		}
	}
	return result
}

// UnserializeMap converts protobuf map to golang map
func UnserializeMap(m map[string]*Values) map[string][]string {
	if m == nil {
		return nil
	}
	result := map[string][]string{}
	for k, v := range m {
		cpy := make([]string, len(v.Arr))
		copy(cpy, v.Arr)
		result[k] = cpy
	}
	return result
}

func marshalHTTPRequest(backendURL string, r *http.Request) ([]byte, error) {
	req := Request{
		URL:           backendURL,
		Method:        r.Method,
		Header:        SerializeMap(r.Header),
		Form:          SerializeMap(r.Form),
		RemoteAddr:    r.RemoteAddr,
		TransportInfo: &TransportInfo{Sequence: 0},
	}

	b := make([]byte, 1024*32)
	buf := bytes.NewBuffer(b)
	buf.Reset() // Empty buffer.
	// TODO: check that it doesn't exceed 1MB which will overflow nats.
	// If we chunk a request, we will need to verify that goes to exactly one agent instance.
	// If we were in the middle of a restart, maybe a pod would be starting up before the old was shut down.
	// For HA in the future this will also likely be a minor concern.
	if r.Body != nil {
		if _, err := io.Copy(buf, r.Body); err != nil {
			return nil, err
		}
		if err := r.Body.Close(); err != nil {
			return nil, err
		}
	}

	// Put on request
	req.Body = buf.Bytes()
	return proto.Marshal(&req)
}

// unmarshalHTTPRequest convert nats message payload to http request
func unmarshalHTTPRequest(ctx context.Context, request *Request) (*http.Request, error) {
	var body io.Reader
	if request.Body != nil {
		body = bytes.NewBuffer(request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, request.GetMethod(), request.GetURL(), body)
	if err != nil {
		return nil, err
	}
	req.Header = UnserializeMap(request.GetHeader())

	return req, nil
}

// SetupConnOptions sets up defaults for nats connection
func SetupConnOptions(opts []nats.Option) []nats.Option {
	// TODO: this method really angers the linter and is just a copy from nats example client - cleanup.
	totalWait := 10 * time.Minute
	reconnectDelay := time.Second

	opts = append(opts, nats.ReconnectWait(reconnectDelay), nats.Timeout(2*time.Second)) // nolint: gocritic
	opts = append(opts, nats.MaxReconnects(int(totalWait/reconnectDelay)))               // nolint: gocritic
	opts = append(opts, nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {       // nolint: gocritic
		logrus.Infof("SetupConnOptions.Disconnected due to: %s, will attempt reconnects for %.0fm", err, totalWait.Minutes())
	}))
	opts = append(opts, nats.ReconnectHandler(func(nc *nats.Conn) { // nolint: gocritic
		logrus.Infof("SetupConnOptions.Reconnected [%s]", nc.ConnectedUrl())
	}))
	opts = append(opts, nats.ClosedHandler(func(nc *nats.Conn) { // nolint: gocritic
		logrus.Infof("SetupConnOptions.Exiting: %v", nc.LastError())
	}))
	return opts
}
