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
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/gitprotocolio"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	gitBinary string
	// *managedRepository map keyed by a cached repository path.
	managedRepos sync.Map
)

func init() {
	var err error
	gitBinary, err = exec.LookPath("git")
	if err != nil {
		log.Fatal("Cannot find the git binary: ", err)
	}
}

func getManagedRepo(localDiskPath string, u *url.URL, config *ServerConfig) *managedRepository {
	newM := &managedRepository{
		localDiskPath: localDiskPath,
		upstreamURL:   u,
		config:        config,
	}
	newM.mu.Lock()
	m, loaded := managedRepos.LoadOrStore(localDiskPath, newM)
	ret := m.(*managedRepository)
	if !loaded {
		ret.mu.Unlock()
	}
	return ret
}

func openManagedRepository(config *ServerConfig, u *url.URL) (*managedRepository, error) {
	u, err := config.URLCanonializer(u)
	if err != nil {
		return nil, err
	}

	localDiskPath := filepath.Join(config.LocalDiskCacheRoot, u.Host, u.Path)

	m := getManagedRepo(localDiskPath, u, config)
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := os.Stat(localDiskPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, status.Errorf(codes.Internal, "error while initializing local Git repoitory: %v", err)
		}

		if err := os.MkdirAll(localDiskPath, 0750); err != nil {
			return nil, status.Errorf(codes.Internal, "cannot create a cache dir: %v", err)
		}

		op := noopOperation{}
		runGit(op, localDiskPath, "init", "--bare")
		runGit(op, localDiskPath, "config", "protocol.version", "2")
		runGit(op, localDiskPath, "config", "uploadpack.allowfilter", "1")
		runGit(op, localDiskPath, "config", "uploadpack.allowrefinwant", "1")
		runGit(op, localDiskPath, "config", "repack.writebitmaps", "1")
		// It seems there's a bug in libcurl and HTTP/2 doens't work.
		runGit(op, localDiskPath, "config", "http.version", "HTTP/1.1")
		runGit(op, localDiskPath, "remote", "add", "--mirror=fetch", "origin", u.String())
	}

	return m, nil
}

func logStats(command string, startTime time.Time, err error) {
	code := codes.Unavailable
	if st, ok := status.FromError(err); ok {
		code = st.Code()
	}
	stats.RecordWithTags(context.Background(),
		[]tag.Mutator{
			tag.Insert(CommandTypeKey, command),
			tag.Insert(CommandCanonicalStatusKey, code.String()),
		},
		OutboundCommandCount.M(1),
		OutboundCommandProcessingTime.M(int64(time.Now().Sub(startTime)/time.Millisecond)),
	)
}

type managedRepository struct {
	localDiskPath string
	lastUpdate    time.Time
	upstreamURL   *url.URL
	config        *ServerConfig
	mu            sync.RWMutex
}

func (r *managedRepository) lsRefsUpstream(command []*gitprotocolio.ProtocolV2RequestChunk) ([]*gitprotocolio.ProtocolV2ResponseChunk, error) {
	req, err := http.NewRequest("POST", r.upstreamURL.String()+"/git-upload-pack", newGitRequest(command))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot construct a request object: %v", err)
	}
	t, err := r.config.TokenSource.Token()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot obtain an OAuth2 access token for the server: %v", err)
	}
	req.Header.Add("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Add("Accept", "application/x-git-upload-pack-result")
	req.Header.Add("Git-Protocol", "version=2")
	t.SetAuthHeader(req)

	startTime := time.Now()
	resp, err := http.DefaultClient.Do(req)
	logStats("ls-refs", startTime, err)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot send a request to the upstream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errMessage := ""
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			bs, err := ioutil.ReadAll(resp.Body)
			if err == nil {
				errMessage = string(bs)
			}
		}
		return nil, fmt.Errorf("got a non-OK response from the upstream: %v %s", resp.StatusCode, errMessage)
	}

	chunks := []*gitprotocolio.ProtocolV2ResponseChunk{}
	v2Resp := gitprotocolio.NewProtocolV2Response(resp.Body)
	for v2Resp.Scan() {
		chunks = append(chunks, copyResponseChunk(v2Resp.Chunk()))
	}
	if err := v2Resp.Err(); err != nil {
		return nil, fmt.Errorf("cannot parse the upstream response: %v", err)
	}
	return chunks, nil
}

func (r *managedRepository) fetchUpstream() (err error) {
	op := r.startOperation("FetchUpstream")
	defer func() {
		op.Done(err)
	}()

	// Because of
	// https://public-inbox.org/git/20190915211802.207715-1-masayasuzuki@google.com/T/#t,
	// the initial git-fetch can be very slow. Split the fetch if there's no
	// reference (== an empty repo).
	g, err := git.PlainOpen(r.localDiskPath)
	if err != nil {
		return fmt.Errorf("cannot open the local cached repository: %v", err)
	}
	splitGitFetch := false
	if _, err := g.Reference("HEAD", true); err == plumbing.ErrReferenceNotFound {
		splitGitFetch = true
	}

	var t *oauth2.Token
	startTime := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if splitGitFetch {
		// Fetch heads and changes first.
		t, err = r.config.TokenSource.Token()
		if err != nil {
			err = status.Errorf(codes.Internal, "cannot obtain an OAuth2 access token for the server: %v", err)
			return err
		}
		err = runGit(op, r.localDiskPath, "-c", "http.extraHeader=Authorization: Bearer "+t.AccessToken, "fetch", "--progress", "-f", "-n", "origin", "refs/heads/*:refs/heads/*", "refs/changes/*:refs/changes/*")
	}
	if err == nil {
		t, err = r.config.TokenSource.Token()
		if err != nil {
			err = status.Errorf(codes.Internal, "cannot obtain an OAuth2 access token for the server: %v", err)
			return err
		}
		err = runGit(op, r.localDiskPath, "-c", "http.extraHeader=Authorization: Bearer "+t.AccessToken, "fetch", "--progress", "-f", "origin")
	}
	logStats("fetch", startTime, err)
	if err == nil {
		r.lastUpdate = startTime
	}
	return err
}

func (r *managedRepository) UpstreamURL() *url.URL {
	u := *r.upstreamURL
	return &u
}

func (r *managedRepository) LastUpdateTime() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastUpdate
}

func (r *managedRepository) RecoverFromBundle(bundlePath string) (err error) {
	op := r.startOperation("ReadBundle")
	defer func() {
		op.Done(err)
	}()

	r.mu.Lock()
	defer r.mu.Unlock()
	err = runGit(op, r.localDiskPath, "fetch", "--progress", "-f", bundlePath, "refs/*:refs/*")
	return
}

func (r *managedRepository) WriteBundle(w io.Writer) (err error) {
	op := r.startOperation("CreateBundle")
	defer func() {
		op.Done(err)
	}()
	err = runGitWithStdOut(op, w, r.localDiskPath, "bundle", "create", "-", "--all")
	return
}

func (r *managedRepository) hasAnyUpdate(refs map[string]plumbing.Hash) (bool, error) {
	g, err := git.PlainOpen(r.localDiskPath)
	if err != nil {
		return false, fmt.Errorf("cannot open the local cached repository: %v", err)
	}
	for refName, hash := range refs {
		ref, err := g.Reference(plumbing.ReferenceName(refName), true)
		if err == plumbing.ErrReferenceNotFound {
			return true, nil
		} else if err != nil {
			return false, fmt.Errorf("cannot open the reference: %v", err)
		}
		if ref.Hash() != hash {
			return true, nil
		}
	}
	return false, nil
}

func (r *managedRepository) hasAllWants(hashes []plumbing.Hash, refs []string) (bool, error) {
	g, err := git.PlainOpen(r.localDiskPath)
	if err != nil {
		return false, fmt.Errorf("cannot open the local cached repository: %v", err)
	}

	for _, hash := range hashes {
		if _, err := g.Object(plumbing.AnyObject, hash); err == plumbing.ErrObjectNotFound {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error while looking up an object for want check: %v", err)
		}
	}

	for _, refName := range refs {
		if _, err := g.Reference(plumbing.ReferenceName(refName), true); err == plumbing.ErrReferenceNotFound {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error while looking up a reference for want check: %v", err)
		}
	}

	return true, nil
}

func (r *managedRepository) serveFetchLocal(command []*gitprotocolio.ProtocolV2RequestChunk, w io.Writer) error {
	// If fetch-upstream is running, it's possible that Git returns
	// incomplete set of objects when the refs being fetched is updated and
	// it uses ref-in-want.
	cmd := exec.Command(gitBinary, "upload-pack", "--stateless-rpc", r.localDiskPath)
	cmd.Env = []string{"GIT_PROTOCOL=version=2"}
	cmd.Dir = r.localDiskPath
	cmd.Stdin = newGitRequest(command)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *managedRepository) startOperation(op string) RunningOperation {
	if r.config.LongRunningOperationLogger != nil {
		return r.config.LongRunningOperationLogger(op, r.upstreamURL)
	}
	return noopOperation{}
}

func runGit(op RunningOperation, gitDir string, arg ...string) error {
	cmd := exec.Command(gitBinary, arg...)
	cmd.Env = []string{}
	cmd.Dir = gitDir
	cmd.Stderr = &operationWriter{op}
	cmd.Stdout = &operationWriter{op}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run a git command: %v", err)
	}
	return nil
}

func runGitWithStdOut(op RunningOperation, w io.Writer, gitDir string, arg ...string) error {
	cmd := exec.Command(gitBinary, arg...)
	cmd.Env = []string{}
	cmd.Dir = gitDir
	cmd.Stdout = w
	cmd.Stderr = &operationWriter{op}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run a git command: %v", err)
	}
	return nil
}

func newGitRequest(command []*gitprotocolio.ProtocolV2RequestChunk) io.Reader {
	b := new(bytes.Buffer)
	for _, c := range command {
		b.Write(c.EncodeToPktLine())
	}
	return b
}

type noopOperation struct{}

func (noopOperation) Printf(string, ...interface{}) {}
func (noopOperation) Done(error)                    {}

type operationWriter struct {
	op RunningOperation
}

func (w *operationWriter) Write(p []byte) (int, error) {
	w.op.Printf("%s", string(p))
	return len(p), nil
}
