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

package testing

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/goblet"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	ValidClientAuthToken = "valid-client-auth-token"
	validServerAuthToken = "valid-server-auth-token"
)

var (
	gitBinary string

	TestTokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: validServerAuthToken})
)

func init() {
	var err error
	gitBinary, err = exec.LookPath("git")
	if err != nil {
		log.Fatal("Cannot find the git binary: ", err)
	}
}

type TestServer struct {
	UpstreamGitRepo   GitRepo
	upstreamServer    *httptest.Server
	UpstreamServerURL string
	proxyServer       *httptest.Server
	ProxyServerURL    string
}

type TestServerConfig struct {
	RequestAuthorizer func(r *http.Request) error
	TokenSource       oauth2.TokenSource
	ErrorReporter     func(*http.Request, error)
	RequestLogger     func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration)
}

func NewTestServer(config *TestServerConfig) *TestServer {
	s := &TestServer{}
	{
		s.UpstreamGitRepo = NewLocalBareGitRepo()
		s.UpstreamGitRepo.Run("config", "http.receivepack", "1")
		s.UpstreamGitRepo.Run("config", "uploadpack.allowfilter", "1")
		s.UpstreamGitRepo.Run("config", "receive.advertisepushoptions", "1")

		s.upstreamServer = httptest.NewServer(http.HandlerFunc(s.upstreamServerHandler))
		s.UpstreamServerURL = s.upstreamServer.URL
	}

	{
		dir, err := ioutil.TempDir("", "goblet_cache")
		if err != nil {
			log.Fatal(err)
		}
		config := &goblet.ServerConfig{
			LocalDiskCacheRoot: dir,
			URLCanonializer:    s.testURLCanonicalizer,
			RequestAuthorizer:  config.RequestAuthorizer,
			TokenSource:        config.TokenSource,
			ErrorReporter:      config.ErrorReporter,
			RequestLogger:      config.RequestLogger,
		}
		s.proxyServer = httptest.NewServer(goblet.HTTPHandler(config))
		s.ProxyServerURL = s.proxyServer.URL
	}
	return s
}

func (s *TestServer) testURLCanonicalizer(u *url.URL) (*url.URL, error) {
	ret, err := url.Parse(s.UpstreamServerURL)
	if err != nil {
		return nil, err
	}
	ret.Path = u.Path

	// Git endpoint suffixes.
	if strings.HasSuffix(ret.Path, "/info/refs") {
		ret.Path = strings.TrimSuffix(ret.Path, "/info/refs")
	} else if strings.HasSuffix(ret.Path, "/git-upload-pack") {
		ret.Path = strings.TrimSuffix(ret.Path, "/git-upload-pack")
	} else if strings.HasSuffix(ret.Path, "/git-receive-pack") {
		ret.Path = strings.TrimSuffix(ret.Path, "/git-receive-pack")
	}
	ret.Path = strings.TrimSuffix(ret.Path, ".git")
	return ret, nil
}

func (s *TestServer) upstreamServerHandler(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+validServerAuthToken {
		http.Error(w, "invalid authenticator", http.StatusForbidden)
		return
	}

	h := &cgi.Handler{
		Path: gitBinary,
		Dir:  string(s.UpstreamGitRepo),
		Env: []string{
			"GIT_PROJECT_ROOT=" + string(s.UpstreamGitRepo),
			"GIT_HTTP_EXPORT_ALL=1",
		},
		Args: []string{
			"http-backend",
		},
		Stderr: os.Stderr,
	}
	if p := req.Header.Get("Git-Protocol"); p != "" {
		h.Env = append(h.Env, "GIT_PROTOCOL="+p)
	}
	if len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked" {
		// Not sure why this restriction is in place in the
		// library.
		req.TransferEncoding = nil
	}
	h.ServeHTTP(w, req)
}

func (s *TestServer) CreateRandomCommitUpstream() (string, error) {
	pushClient := NewLocalGitRepo()
	defer pushClient.Close()
	hash, err := pushClient.CreateRandomCommit()
	if err != nil {
		return "", err
	}

	_, err = pushClient.Run("-c", "http.extraHeader=Authorization: Bearer "+validServerAuthToken, "push", "-f", s.UpstreamServerURL, "master:master")
	return hash, err

}

func (s *TestServer) Close() {
	s.upstreamServer.Close()
	s.proxyServer.Close()
	s.UpstreamGitRepo.Close()
}

func TestRequestAuthorizer(r *http.Request) error {
	authzHeader := r.Header.Get("Authorization")
	if authzHeader == "Bearer "+ValidClientAuthToken {
		return nil
	}
	return status.Errorf(codes.Unauthenticated, "not a valid client auth token: %s", authzHeader)
}

type GitRepo string

func NewLocalBareGitRepo() GitRepo {
	dir, err := ioutil.TempDir("", "goblet_tmp")
	if err != nil {
		log.Fatal(err)
	}
	r := GitRepo(dir)
	r.Run("init", "--bare")
	return r
}

func NewLocalGitRepo() GitRepo {
	dir, err := ioutil.TempDir("", "goblet_tmp")
	if err != nil {
		log.Fatal(err)
	}
	r := GitRepo(dir)
	r.Run("init")
	r.Run("config", "user.email", "local-root@example.com")
	r.Run("config", "user.name", "local root")
	r.Run("config", "protocol.version", "2")
	return r
}

func (r GitRepo) Run(arg ...string) (string, error) {
	cmd := exec.Command(gitBinary, arg...)
	cmd.Dir = string(r)
	cmd.Env = []string{}
	bs, err := cmd.CombinedOutput()
	if err != nil {
		return "", &commandError{err, cmd.Args, strings.TrimRight(string(bs), "\n")}
	}
	return string(bs), nil
}

func (r GitRepo) CreateRandomCommit() (string, error) {
	if _, err := r.Run("commit", "--allow-empty", "--message="+time.Now().String()); err != nil {
		return "", err
	}
	return r.Run("rev-parse", "master")
}

func (r GitRepo) Close() error {
	return os.RemoveAll(string(r))
}

type commandError struct {
	err    error
	args   []string
	output string
}

func (c *commandError) Error() string {
	ss := []string{
		"cannot execute a git command",
		fmt.Sprintf("Error: %v", c.err),
		fmt.Sprintf("Args: %#v", c.args),
	}
	for _, s := range strings.Split(c.output, "\n") {
		ss = append(ss, "Output: "+s)
	}
	return strings.Join(ss, "\n")
}
