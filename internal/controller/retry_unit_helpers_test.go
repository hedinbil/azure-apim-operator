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

// Helpers for retry_cases_test.go. Everything here is prefixed "rc" so it cannot clash
// with the helpers of the other retry tests in this package.

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// rcStart is the instant every case starts from: the first day of the Sep 2026 incident.
var rcStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// rcClock is a settable clock.
type rcClock struct{ t time.Time }

func (c *rcClock) now() time.Time          { return c.t }
func (c *rcClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// rcPolicy is the production shape (1m base, 30m cap, 5 attempts) without jitter, on clock.
func rcPolicy(clock *rcClock) *retryPolicy {
	return &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now}
}

// rcTag is an APIMTag with the given retry annotation; "" means no annotation at all.
func rcTag(retryValue string) *apimv1.APIMTag {
	tag := &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{Name: "tag-rc", Namespace: "team-rc", Generation: 3}}
	if retryValue != "" {
		tag.Annotations = map[string]string{retryAnnotation: retryValue}
	}
	return tag
}

// rcAPIMErr builds an *apim.Error as the request helper would for an HTTP answer.
func rcAPIMErr(method string, status int, code, detailCode string) *apim.Error {
	return &apim.Error{Operation: "rc op", Method: method, StatusCode: status, Code: code, DetailCode: detailCode,
		Message: "rc message"}
}

var (
	rcTransientErr = rcAPIMErr(http.MethodPut, http.StatusPreconditionFailed, "PreconditionFailed", "")
	rcPermanentErr = rcAPIMErr(http.MethodPut, http.StatusBadRequest, "ValidationError", "")
)

// rcLine is one captured log call.
type rcLine struct {
	msg   string
	isErr bool
	err   error
	kv    map[string]any
}

// rcLogs returns a logger that records every line, and a func returning what it recorded.
func rcLogs() (logr.Logger, func() []rcLine) {
	sink := &rcSink{mu: &sync.Mutex{}, lines: &[]rcLine{}}
	return logr.New(sink), func() []rcLine {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return append([]rcLine(nil), (*sink.lines)...)
	}
}

// rcSink is a minimal logr.LogSink keeping message, error and keys.
type rcSink struct {
	mu     *sync.Mutex
	lines  *[]rcLine
	values []any
}

func (s *rcSink) Init(logr.RuntimeInfo)        {}
func (s *rcSink) Enabled(int) bool             { return true }
func (s *rcSink) WithName(string) logr.LogSink { return s }
func (s *rcSink) WithValues(kv ...any) logr.LogSink {
	return &rcSink{mu: s.mu, lines: s.lines, values: append(append([]any(nil), s.values...), kv...)}
}
func (s *rcSink) Info(_ int, msg string, kv ...any)      { s.emit(msg, false, nil, kv) }
func (s *rcSink) Error(err error, msg string, kv ...any) { s.emit(msg, true, err, kv) }

func (s *rcSink) emit(msg string, isErr bool, err error, kv []any) {
	all := append(append([]any(nil), s.values...), kv...)
	m := map[string]any{}
	for i := 0; i+1 < len(all); i += 2 {
		m[fmt.Sprint(all[i])] = all[i+1]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.lines = append(*s.lines, rcLine{msg: msg, isErr: isErr, err: err, kv: m})
}
