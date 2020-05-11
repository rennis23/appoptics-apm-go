// +build go1.7
// Copyright (C) 2016 Librato, Inc. All rights reserved.
// AppOptics HTTP instrumentation for Go

package http

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/reporter"
	"github.com/appoptics/appoptics-apm-go/v1/ao/opentelemetry"
	"go.opentelemetry.io/otel/api/global"
	"go.opentelemetry.io/otel/api/propagation"
	"go.opentelemetry.io/otel/api/trace"
)

const (
	// XTraceHeader is a constant for the HTTP header used by AppOptics ("X-Trace") to propagate
	// the distributed tracing context across HTTP requests.
	XTraceHeader = "X-Trace"
	// Deprecated: use XTraceHeader
	HTTPHeaderName = XTraceHeader
	// XTraceOptionsHeader is a constant for the HTTP header to propagate X-Trace-Options
	// values. It's for trigger trace requests and may be used for other purposes in the future.
	XTraceOptionsHeader = reporter.HTTPHeaderXTraceOptions
	// Deprecated: use XTraceOptionsHeader
	HTTPHeaderXTraceOptions = XTraceOptionsHeader
	// XTraceOptionsSignatureHeader is a constant for the HTTP headers to propagate
	// X-Trace-Options-Signature values. It contains the response codes for X-Trace-Options
	XTraceOptionsSignatureHeader = reporter.HTTPHeaderXTraceOptionsSignature
	// Deprecated: use XTraceOptionsSignatureHeader
	HTTPHeaderXTraceOptionsSignature = XTraceOptionsSignatureHeader
	httpHandlerSpanName              = "http.HandlerFunc"
)

// key used for HTTP span to indicate a new context
var httpSpanKey = ao.ContextKeyT("github.com/appoptics/appoptics-apm-go/v1/ao.HTTPSpan")

// Handler wraps an http.HandlerFunc with entry / exit events,
// returning a new handler that can be used in its place.
//   http.HandleFunc("/path", ao.Handler(myHandler))
func Handler(handler func(http.ResponseWriter, *http.Request),
	opts ...ao.SpanOpt) func(http.ResponseWriter, *http.Request) {
	// At wrap time (when binding handler to router): get name of wrapped handler func
	var endArgs []interface{}
	if f := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()); f != nil {
		// e.g. "main.slowHandler", "github.com/appoptics/appoptics-apm-go/v1/ao_test.handler404"
		fname := f.Name()
		if s := strings.SplitN(fname[strings.LastIndex(fname, "/")+1:], ".", 2); len(s) == 2 {
			endArgs = append(endArgs, "Controller", s[0], "Action", s[1])
		}
	}
	// return wrapped HTTP request handler
	return func(w http.ResponseWriter, r *http.Request) {
		if ao.Closed() {
			handler(w, r)
			return
		}

		t, w, r := TraceFromHTTPRequestResponse(httpHandlerSpanName, w, r, opts...)
		defer t.End(endArgs...)

		defer func() { // catch and report panic, if one occurs
			if err := recover(); err != nil {
				t.Error("panic", fmt.Sprintf("%v", err))
				panic(err) // re-raise the panic
			}
		}()
		// Call original HTTP handler
		handler(w, r)
	}
}

// TraceFromHTTPRequestResponse returns a Trace, a wrapped http.ResponseWriter, and a modified
// http.Request, given a http.ResponseWriter and http.Request. If a distributed trace is described
// in the HTTP request headers, the trace's context will be continued. The returned http.ResponseWriter
// should be used in place of the one passed into this function in order to observe the response's
// headers and status code.
//   func myHandler(w http.ResponseWriter, r *http.Request) {
//       tr, w, r := ao.TraceFromHTTPRequestResponse("myHandler", w, r)
//       defer tr.End()
//       // ...
//   }
func TraceFromHTTPRequestResponse(spanName string, w http.ResponseWriter, r *http.Request,
	opts ...ao.SpanOpt) (ao.Trace, http.ResponseWriter, *http.Request) {

	// determine if this is a new context, if so set flag isNewContext to start a new HTTP Span
	isNewContext := false
	if b, ok := r.Context().Value(httpSpanKey).(bool); !ok || !b {
		// save KV to ensure future calls won't treat as new context
		r = r.WithContext(context.WithValue(r.Context(), httpSpanKey, true))
		isNewContext = true
	}

	t := traceFromHTTPRequest(spanName, r, isNewContext, opts...)

	// Associate the trace with http.Request to expose it to the handler
	r = r.WithContext(ao.NewContext(r.Context(), t))

	wrapper := newResponseWriter(w, t) // wrap writer with response-observing writer
	for k, v := range t.HTTPRspHeaders() {
		wrapper.Header().Set(k, v)
	}

	return t, wrapper, r
}

// ResponseWriter observes an http.ResponseWriter when WriteHeader() or Write() is called to
// check the status code and response headers.
type ResponseWriter struct {
	Writer      http.ResponseWriter
	t           ao.Trace
	StatusCode  int
	WroteHeader bool
}

// Deprecated: use ResponseWriter
type HTTPResponseWriter = ResponseWriter

func (w *ResponseWriter) Write(p []byte) (n int, err error) {
	if !w.WroteHeader {
		w.WriteHeader(w.StatusCode)
	}
	return w.Writer.Write(p)
}

// Header implements the http.ResponseWriter interface.
func (w *ResponseWriter) Header() http.Header { return w.Writer.Header() }

// WriteHeader implements the http.ResponseWriter interface.
func (w *ResponseWriter) WriteHeader(status int) {
	w.StatusCode = status              // observe HTTP status code
	md := w.Header().Get(XTraceHeader) // check response for downstream metadata
	if w.t.IsReporting() {             // set trace exit metadata in X-Trace header
		// if downstream response headers mention a different span, add edge to it
		if md != "" && md != w.t.ExitMetadata() {
			w.t.AddEndArgs(ao.KeyEdge, md)
		}
		w.Header().Set(XTraceHeader, w.t.ExitMetadata()) // replace downstream MD with ours
	}
	w.WroteHeader = true
	w.Writer.WriteHeader(status)
}

// newResponseWriter observes the HTTP Status code of an HTTP response, returning a
// wrapped http.ResponseWriter and a pointer to an int containing the status.
func newResponseWriter(writer http.ResponseWriter, t ao.Trace) *ResponseWriter {
	w := &ResponseWriter{Writer: writer, t: t, StatusCode: http.StatusOK}
	t.AddEndArgs(ao.KeyStatus, &w.StatusCode)
	// add exit event metadata to X-Trace header
	if t.IsReporting() {
		// add/replace response header metadata with this trace's
		w.Header().Set(XTraceHeader, t.ExitMetadata())
	}
	return w
}

// traceFromHTTPRequest returns a Trace, given an http.Request. If a distributed trace is described
// in the "X-Trace" header, this context will be continued.
func traceFromHTTPRequest(spanName string, r *http.Request, isNewContext bool, opts ...ao.SpanOpt) ao.Trace {
	so := &ao.SpanOptions{}
	for _, f := range opts {
		f(so)
	}

	mdStr := r.Header.Get(XTraceHeader)
	// Get OT trace context
	if mdStr == "" {
		ctx := propagation.ExtractHTTP(context.Background(), global.Propagators(), r.Header)
		otSpanContext := trace.RemoteSpanContextFromContext(ctx)
		mdStr = opentelemetry.OTSpanContext2MdStr(otSpanContext)
	}

	// start trace, passing in metadata header
	t := ao.NewTraceWithOptions(spanName, ao.SpanOptions{
		WithBackTrace: false,
		StartTime:     time.Time{},
		EndTime:       time.Time{},
		ContextOptions: reporter.ContextOptions{
			MdStr:                  mdStr,
			URL:                    r.URL.EscapedPath(),
			XTraceOptions:          r.Header.Get(XTraceOptionsHeader),
			XTraceOptionsSignature: r.Header.Get(XTraceOptionsSignatureHeader),
			CB: func() ao.KVMap {
				kvs := ao.KVMap{
					ao.KeyMethod:      r.Method,
					ao.KeyHTTPHost:    r.Host,
					ao.KeyURL:         r.URL.EscapedPath(),
					ao.KeyRemoteHost:  r.RemoteAddr,
					ao.KeyQueryString: r.URL.RawQuery,
				}

				if so.WithBackTrace {
					kvs[ao.KeyBackTrace] = string(debug.Stack())
				}

				return kvs
			}},
	})

	// set the start time and method for metrics collection
	t.SetMethod(r.Method)
	t.SetPath(r.URL.EscapedPath())

	var host string
	if host = r.Header.Get("X-Forwarded-Host"); host == "" {
		host = r.Host
	}
	t.SetHost(host)

	// Clear the start time if it is not a new context
	if !isNewContext {
		t.SetStartTime(time.Time{})
	}

	// update incoming metadata in request headers for any downstream readers
	r.Header.Set(XTraceHeader, t.MetadataString())
	return t
}
