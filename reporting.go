// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package goblet

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	serverErrorCodes = map[codes.Code]bool{
		codes.DataLoss:         true,
		codes.DeadlineExceeded: true,
		codes.Internal:         true,
		codes.Unavailable:      true,
		codes.Unknown:          true,
	}
)

type httpErrorReporter struct {
	config *ServerConfig
	req    *http.Request
	w      http.ResponseWriter
}

func (h *httpErrorReporter) reportError(err error) {
	code := codes.Internal
	message := ""
	if st, ok := status.FromError(err); ok {
		code = st.Code()
		message = st.Message()
	}
	stats.RecordWithTags(
		h.req.Context(),
		[]tag.Mutator{tag.Insert(CommandCanonicalStatusKey, code.String())},
		InboundCommandCount.M(1),
	)

	if code == codes.Unauthenticated {
		h.w.Header().Add("WWW-Authenticate", "Bearer")
		h.w.Header().Add("WWW-Authenticate", "Basic realm=goblet")
	}
	httpStatus := HTTPStatusFromGrpcCode(code)
	if message == "" {
		message = http.StatusText(httpStatus)
	}
	http.Error(h.w, message, httpStatus)

	if !serverErrorCodes[code] {
		return
	}

	if h.config.ErrorReporter != nil {
		h.config.ErrorReporter(h.req, err)
		return
	}
	log.Printf("Error while processing a request: %v", err)
}

type gitProtocolHTTPErrorReporter struct {
	config *ServerConfig
	req    *http.Request
	w      http.ResponseWriter
}

func (h *gitProtocolHTTPErrorReporter) reportError(ctx context.Context, startTime time.Time, err error) {
	code := codes.Internal
	if st, ok := status.FromError(err); ok {
		code = st.Code()
	}
	stats.RecordWithTags(
		ctx,
		[]tag.Mutator{tag.Insert(CommandCanonicalStatusKey, code.String())},
		InboundCommandCount.M(1),
		InboundCommandProcessingTime.M(int64(time.Now().Sub(startTime)/time.Millisecond)),
	)

	if err != nil {
		writeError(h.w, err)
	}

	if !serverErrorCodes[code] {
		return
	}

	if h.config.ErrorReporter != nil {
		h.config.ErrorReporter(h.req.WithContext(ctx), err)
		return
	}
	log.Printf("Error while processing a request: %v", err)
}

func logHTTPRequest(config *ServerConfig, w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	startTime := time.Now()
	monR := &monitoringReader{r: r.Body}
	r.Body = monR

	monW := &monitoringWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		monW.flush = f.Flush
	} else {
		monW.flush = func() {}
	}

	return monW, func() {
		if config.RequestLogger == nil {
			return
		}
		endTime := time.Now()

		config.RequestLogger(r, monW.status, monR.bytesRead, monW.bytesWritten, endTime.Sub(startTime))
	}
}

type monitoringReader struct {
	r         io.ReadCloser
	bytesRead int64
}

func (r *monitoringReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func (r *monitoringReader) Close() error {
	return r.r.Close()
}

type monitoringWriter struct {
	status       int
	w            http.ResponseWriter
	flush        func()
	bytesWritten int64
}

func (w *monitoringWriter) Flush() {
	w.flush()
}

func (w *monitoringWriter) Write(bs []byte) (n int, err error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err = w.w.Write(bs)
	w.bytesWritten += int64(len(bs))
	w.flush()
	return
}

func (w *monitoringWriter) WriteHeader(status int) {
	w.status = status
	w.w.WriteHeader(status)
}

func (w *monitoringWriter) Header() http.Header {
	return w.w.Header()
}
