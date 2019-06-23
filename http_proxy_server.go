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
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/google/gitprotocolio"
	"go.opencensus.io/tag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type httpProxyServer struct {
	config *ServerConfig
}

func (s *httpProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, logCloser := logHTTPRequest(s.config, w, r)
	defer logCloser()
	reporter := &httpErrorReporter{config: s.config, req: r, w: w}

	ctx, err := tag.New(r.Context(), tag.Insert(CommandTypeKey, "not-a-command"))
	if err != nil {
		reporter.reportError(err)
		return
	}
	r = r.WithContext(ctx)

	// Technically, this server is an HTTP proxy, and it should use
	// Proxy-Authorization / Proxy-Authenticate. However, existing
	// authentication mechanism around Git is not compatible with proxy
	// authorization. We use normal authentication mechanism here.
	if err := s.config.RequestAuthorizer(r); err != nil {
		reporter.reportError(err)
		return
	}
	if proto := r.Header.Get("Git-Protocol"); proto != "version=2" {
		reporter.reportError(status.Error(codes.InvalidArgument, "accepts only Git protocol v2"))
		return
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/info/refs"):
		s.infoRefsHandler(reporter, w, r)
	case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		reporter.reportError(status.Error(codes.Unimplemented, "git-receive-pack not supported"))
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		s.uploadPackHandler(reporter, w, r)
	}
}

func (s *httpProxyServer) infoRefsHandler(reporter *httpErrorReporter, w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("service") != "git-upload-pack" {
		reporter.reportError(status.Error(codes.InvalidArgument, "accepts only git-fetch"))
		return
	}

	w.Header().Add("Content-Type", "application/x-git-upload-pack-advertisement")
	rs := []*gitprotocolio.InfoRefsResponseChunk{
		{ProtocolVersion: 2},
		{Capabilities: []string{"ls-refs"}},
		// See managed_repositories.go for not having ref-in-want.
		{Capabilities: []string{"fetch=filter shallow"}},
		{Capabilities: []string{"server-option"}},
		{EndOfRequest: true},
	}
	for _, pkt := range rs {
		if err := writePacket(w, pkt); err != nil {
			// Client-side IO error. Treat this as Canceled.
			reporter.reportError(status.Errorf(codes.Canceled, "client IO error"))
			return
		}
	}
}

func (s *httpProxyServer) uploadPackHandler(reporter *httpErrorReporter, w http.ResponseWriter, r *http.Request) {
	// /git-upload-pack doesn't recognize text/plain error. Send an error
	// with ErrorPacket.
	w.Header().Add("Content-Type", "application/x-git-upload-pack-result")
	if r.Header.Get("Content-Encoding") == "gzip" {
		var err error
		if r.Body, err = gzip.NewReader(r.Body); err != nil {
			reporter.reportError(status.Errorf(codes.InvalidArgument, "cannot ungzip: %v", err))
			return
		}
	}

	// HTTP is strictly speaking a request-response protocol, and a server
	// cannot send a non-error response until the entire request is read.
	// We need to compromise and either drain the entire request first or
	// buffer the entire response.
	//
	// Because this server supports only ls-refs and fetch commands, valid
	// protocol V2 requests are relatively small in practice compared to the
	// response. A request with many wants and haves can be large, but
	// practically there's a limit on the number of haves a client would
	// send. Compared to that the fetch response can contain a packfile, and
	// this can easily get large. Read the entire request upfront.
	commands, err := parseAllCommands(r.Body)
	if err != nil {
		reporter.reportError(err)
		return
	}

	repo, err := openManagedRepository(s.config, r.URL)
	if err != nil {
		reporter.reportError(err)
		return
	}

	gitReporter := &gitProtocolHTTPErrorReporter{config: s.config, req: r, w: w}
	for _, command := range commands {
		if !handleV2Command(r.Context(), gitReporter, repo, command, w) {
			return
		}
	}
}

func parseAllCommands(r io.Reader) ([][]*gitprotocolio.ProtocolV2RequestChunk, error) {
	commands := [][]*gitprotocolio.ProtocolV2RequestChunk{}
	v2Req := gitprotocolio.NewProtocolV2Request(r)
	for {
		chunks := []*gitprotocolio.ProtocolV2RequestChunk{}
		for v2Req.Scan() {
			c := copyRequestChunk(v2Req.Chunk())
			if c.EndRequest {
				break
			}
			chunks = append(chunks, c)
		}
		if len(chunks) == 0 || v2Req.Err() != nil {
			break
		}

		switch chunks[0].Command {
		case "ls-refs":
		case "fetch":
			// Do nothing.
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unrecognized command: %v", chunks[0])
		}
		commands = append(commands, chunks)
	}

	if err := v2Req.Err(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot parse the request: %v", err)
	}
	return commands, nil
}
