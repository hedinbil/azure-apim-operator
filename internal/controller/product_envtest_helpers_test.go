/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// peReply is one scripted ARM answer for the APIMProduct envtest specs.
type peReply struct {
	status int
	body   string
}

// peOK is a plain 200 with an empty JSON object.
var peOK = peReply{status: http.StatusOK, body: `{}`}

// peARMError is ARM's JSON error shape: {"error":{"code":..,"message":..}}.
func peARMError(status int, code, message string) peReply {
	return peReply{status: status, body: fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)}
}

// peARMErrorWithDetail is ARM's JSON error shape with one detail, whose code is what
// the classification looks at as error.details[0].code.
func peARMErrorWithDetail(status int, code, detailCode, message string) peReply {
	return peReply{status: status, body: fmt.Sprintf(
		`{"error":{"code":%q,"message":%q,"details":[{"code":%q,"message":"detail"}]}}`,
		code, message, detailCode)}
}

// peRequest is one request the fake ARM received.
type peRequest struct {
	method      string
	path        string
	query       string
	auth        string
	ifMatch     string
	contentType string
	body        string
}

// peFakeARM stands in for Azure Resource Manager in the APIMProduct envtest specs. Each
// HTTP method has a sticky reply (200 {} until set) and every request is recorded.
type peFakeARM struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  map[string]peReply
	requests []peRequest
}

func newPEFakeARM() *peFakeARM {
	f := &peFakeARM{replies: map[string]peReply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, peRequest{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.RawQuery,
			auth:        r.Header.Get("Authorization"),
			ifMatch:     r.Header.Get("If-Match"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(body),
		})
		reply, ok := f.replies[r.Method]
		f.mu.Unlock()
		if !ok {
			reply = peOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	return f
}

// set makes every following request with method get reply.
func (f *peFakeARM) set(method string, reply peReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = reply
}

// count returns how many requests with method ARM has seen.
func (f *peFakeARM) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method {
			n++
		}
	}
	return n
}

// last returns the most recent request with method; ok is false when there was none.
func (f *peFakeARM) last(method string) (req peRequest, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].method == method {
			return f.requests[i], true
		}
	}
	return peRequest{}, false
}

// peLinesWith returns the captured log lines whose message is exactly msg.
func peLinesWith(lines []logLine, msg string) []logLine {
	var out []logLine
	for _, line := range lines {
		if line.msg == msg {
			out = append(out, line)
		}
	}
	return out
}
