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
	"errors"
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
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/alitto/pond"
	"github.com/google/gitprotocolio"
	git "github.com/libgit2/git2go/v33"
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

	ErrReferenceNotFound   = errors.New("reference not found")
	ErrReferenceInvalid    = errors.New("reference is not valid")
	serveFetchLocalCounter int32
	StatsdClient           *statsd.Client
)

func init() {
	var err error

	gitBinary, err = exec.LookPath("git")
	if err != nil {
		log.Fatal("Cannot find the git binary: ", err)
	}

	StatsdClient, err = statsd.New("127.0.0.1:8125")
	if err != nil {
		log.Fatal("Cannot initialize statsd client: ", err)
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
		log.Printf("FetchUpstreamPool created for %s\n", localDiskPath)
		ret.fetchUpstreamPool = pond.New(1, 1000, pond.IdleTimeout(5*time.Minute), pond.PanicHandler(func(p interface{}) {
			log.Printf("Fetch upstream task panicked: %v\n", p)
		}))

		log.Printf("ServeFetchPool created for %s\n", localDiskPath)
		ret.serveFetchPool = pond.New(100, 1000, pond.IdleTimeout(5*time.Minute), pond.PanicHandler(func(p interface{}) {
			log.Printf("Serve fetch task panicked: %v\n", p)
		}))

		ret.mu.Unlock()
	}
	return ret
}

func openManagedRepository(config *ServerConfig, u *url.URL) (*managedRepository, error) {
	u, err := config.URLCanonicalizer(u)
	if err != nil {
		return nil, err
	}

	localDiskPath := filepath.Join(config.LocalDiskCacheRoot, u.Host, u.Path)

	m := getManagedRepo(localDiskPath, u, config)

	m.once.Do(func() {
		log.Printf("Initializing local Git repository %s\n", localDiskPath)
		if _, err := os.Stat(localDiskPath); err != nil {
			if !os.IsNotExist(err) {
				log.Fatalf("error while initializing local Git repository %s: %v", localDiskPath, err)
			}

			if err := os.MkdirAll(localDiskPath, 0750); err != nil {
				log.Fatalf("cannot create a cache dir %s: %v", localDiskPath, err)
			}

			op := noopOperation{}
			var gitVersionBuilder strings.Builder
			runGitWithStdOut(op, &gitVersionBuilder, localDiskPath, "--version")
			gitVersion := strings.TrimPrefix(strings.TrimSpace(gitVersionBuilder.String()), "git version ")
			userAgent := fmt.Sprintf("git/%s goblet/1.0", gitVersion)

			runGit(op, localDiskPath, "init", "--bare")
			runGit(op, localDiskPath, "config", "protocol.version", "2")
			runGit(op, localDiskPath, "config", "uploadpack.allowfilter", "1")
			runGit(op, localDiskPath, "config", "uploadpack.allowrefinwant", "1")
			runGit(op, localDiskPath, "config", "repack.writebitmaps", "1")
			runGit(op, localDiskPath, "config", "http.userAgent", userAgent)
			runGit(op, localDiskPath, "config", "http.version", "HTTP/2")
			runGit(op, localDiskPath, "remote", "add", "--mirror=fetch", "origin", u.String())

			log.Printf("Created and configured local Git repository (git:%s, dir:%s)\n", gitVersion, localDiskPath)
		} else {
			log.Printf("Local Git repository %s already exists. Skipped configuration\n", localDiskPath)
		}
	})

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
		OutboundCommandProcessingTime.M(int64(time.Since(startTime)/time.Millisecond)),
	)
}

func logElapsed(operation string, startTime time.Time, threshold time.Duration, localDiskPath string) {
	elapsed := time.Since(startTime)
	if elapsed > threshold {
		log.Printf("%s took too long (%s %s)\n", operation, elapsed, localDiskPath)
	}

	tags := []string{
		"dir:" + localDiskPath,
		"op:" + strings.ToLower(strings.ReplaceAll(operation, " ", "_")),
	}
	StatsdClient.Timing("goblet.operation.time", elapsed, tags, 1)
}

type managedRepository struct {
	localDiskPath     string
	lastUpdateUnix    int64
	upstreamURL       *url.URL
	config            *ServerConfig
	mu                sync.RWMutex
	once              sync.Once
	fetchUpstreamPool *pond.WorkerPool
	serveFetchPool    *pond.WorkerPool
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
	logElapsed("lsRefsUpstream", startTime, time.Minute, r.localDiskPath)
	if err != nil {
		log.Printf("ls-refs request failed (dir:%s, err:%v)\n", r.localDiskPath, err)
		return nil, status.Errorf(codes.Internal, "cannot send a request to the upstream: %v", err)
	}
	defer resp.Body.Close()

	// Log response headers
	log.Printf("ls-refs response (dir:%s, status:%d, headers:%v)\n", r.localDiskPath, resp.StatusCode, resp.Header)

	if resp.StatusCode != http.StatusOK {
		errMessage := ""
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			bs, err := ioutil.ReadAll(resp.Body)
			if err == nil {
				errMessage = string(bs)
			}
		}
		errData, _ := ioutil.ReadAll(resp.Body)
		errMessage = string(errData)
		log.Printf("ls-refs failed with non-OK response (dir:%s, status:%d, content-type:%s, error:%s)\n",
			r.localDiskPath, resp.StatusCode, resp.Header.Get("Content-Type"), errMessage)
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

// Run git gc
func (r *managedRepository) runGC() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	op := r.startOperation("gitGC")
	err := runGit(op, r.localDiskPath, "gc", "--prune='15.minutes.ago'")
	op.Done(err)

	return err
}

func (r *managedRepository) fetchUpstream(additionalWants []git.Oid) (err error) {
	var t *oauth2.Token
	lockTime := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	logStats("fetchBlocked", lockTime, nil)

	startTime := time.Now()
	defer logElapsed("fetchUpstream", startTime, time.Minute, r.localDiskPath)

	staled := startTime.Sub(r.LastUpdateTime())
	if staled < time.Hour*24*365 {
		// do not report if it's more than one year old
		StatsdClient.Gauge("goblet.stale.seconds", staled.Seconds(), []string{"dir:" + r.localDiskPath}, 1)
		if staled > time.Hour {
			log.Printf("Repo has staled for %s (dir:%s)\n", staled, r.localDiskPath)
		}
	}

	t, err = r.config.TokenSource.Token()
	if err != nil {
		err = status.Errorf(codes.Internal, "cannot obtain an OAuth2 access token for the server: %v", err)
		return err
	}
	err = r.fetchUpstreamInternal("origin", t, additionalWants)
	endTime := time.Now()
	duration := endTime.Sub(startTime)
	StatsdClient.Distribution("goblet.fetchupstream.dist", duration.Seconds(), []string{"dir:" + r.localDiskPath}, 1)

	log.Printf("FetchUpstream finished (lock:%s, run:%s, err:%v, dir:%s)\n", startTime.Sub(lockTime), duration, err, r.localDiskPath)

	logStats("fetch", startTime, err)
	if err == nil {
		atomic.StoreInt64(&r.lastUpdateUnix, startTime.Unix())
	} else {
		log.Printf("FetchUpstream failed. (token_exp:%s, dir:%s, err:%v)\n", t.Expiry, r.localDiskPath, err)
	}

	return err
}

// This function should not be called directly, call fetchUpstream() instead
func (r *managedRepository) fetchUpstreamInternal(remote string, token *oauth2.Token, additionalWants []git.Oid) error {
	args := make([]string, 0)

	// git options
	args = append(args, "-c")
	args = append(args, fmt.Sprintf("http.extraHeader=Authorization: %s %s", token.Type(), token.AccessToken))
	tokenArgIndex := len(args) - 1

	// git command
	args = append(args, "fetch")

	// fetch options
	args = append(args, "--force")
	args = append(args, "--no-write-fetch-head")
	args = append(args, "--prune")
	args = append(args, "--no-tags")

	// remote
	args = append(args, remote)

	// refspecs
	args = append(args, "+refs/heads/*:refs/remotes/origin/*")
	args = append(args, "^refs/pull/*")
	if len(additionalWants) >= 1 {
		// only the first want will be appended
		args = append(args, additionalWants[0].String())
		if len(additionalWants) > 1 {
			log.Printf("Additional wants %s... ignored in git fetch refspec\n", additionalWants[1].String())
		}
	}

	op := r.startOperation("FetchUpstream")
	err := runGit(op, r.localDiskPath, args...)
	op.Done(err)

	// mask token before logging
	args[tokenArgIndex] = "[redacted]"
	if err != nil {
		log.Printf("FetchUpstream git command failed (cmd:git %s, dir:%s, err:%v)\n", strings.Join(args, " "), r.localDiskPath, err)
	} else {
		log.Printf("FetchUpstream git command succeeded (cmd:git %s, dir:%s)\n", strings.Join(args, " "), r.localDiskPath)
	}

	return err
}

func (r *managedRepository) UpstreamURL() *url.URL {
	u := *r.upstreamURL
	return &u
}

func (r *managedRepository) LastUpdateTime() time.Time {
	lastUpdateUnix := atomic.LoadInt64(&r.lastUpdateUnix)
	return time.Unix(lastUpdateUnix, 0)
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

func (r *managedRepository) hasAnyUpdate(refs map[string]git.Oid) (bool, error) {
	var err error
	startTime := time.Now()
	defer logStats("hasAnyUpdate", startTime, err)
	defer logElapsed("hasAnyUpdate", startTime, 2*time.Second, r.localDiskPath)

	// log.Printf("Comparing refs of %d\n", len(refs))

	repo, err := git.OpenRepository(r.localDiskPath)
	if err != nil {
		return false, fmt.Errorf("cannot open the local cached repository: %v", err)
	}
	defer repo.Free()

	odb, err := repo.Odb()
	if err != nil {
		return false, fmt.Errorf("cannot open odb: %v", err)
	}
	defer odb.Free()

	for refName, expectedHash := range refs {
		ref, err := lookupReference(repo, refName, true)
		if err == ErrReferenceNotFound {
			return true, nil
		} else if err != nil {
			return false, fmt.Errorf("cannot open the reference: %v", err)
		}
		if *ref.Target() != expectedHash {
			if odb.Exists(&expectedHash) {
				// If the expectedHash exists in local repo, it means the local repo is actually ahead of the refs pair
				// In this case, hasAnyUpdate should return false and FetchUpstream is not necessary.
				log.Printf("Comparing and hash not matched but exists %s %s %s\n", refName, expectedHash.String(), ref.Target().String())
			} else {
				log.Printf("Comparing and hash not matched %s %s %s\n", refName, expectedHash.String(), ref.Target().String())
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *managedRepository) hasAllWants(hashes []git.Oid, refs []string) (bool, error) {
	var err error
	startTime := time.Now()
	defer logStats("hasAllWants", startTime, err)
	defer logElapsed("hasAllWants", startTime, 2*time.Second, r.localDiskPath)

	// log.Printf("Searching hashes of %d and refs of %d\n", len(hashes), len(refs))

	repo, err := git.OpenRepository(r.localDiskPath)
	if err != nil {
		return false, fmt.Errorf("cannot open the local cached repository: %v", err)
	}
	defer repo.Free()

	odb, err := repo.Odb()
	if err != nil {
		return false, fmt.Errorf("cannot open odb: %v", err)
	}
	defer odb.Free()

	for _, hash := range hashes {
		if !odb.Exists(&hash) {
			// log.Printf("Searching hash and not found %s\n", hash.String())
			return false, nil
		} else {
			// log.Printf("Searching hash and found %s\n", hash.String())
		}
	}

	for _, refName := range refs {
		if _, err := lookupReference(repo, refName, true); err == ErrReferenceNotFound {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error while looking up a reference for want check: %v", err)
		}
	}

	return true, nil
}

func (r *managedRepository) serveFetchLocal(command []*gitprotocolio.ProtocolV2RequestChunk, w io.Writer, ci_source string) error {
	// If fetch-upstream is running, it's possible that Git returns
	// incomplete set of objects when the refs being fetched is updated and
	// it uses ref-in-want.

	args := make([]string, 0)
	env := []string{"GIT_PROTOCOL=version=2"}

	if r.config.PackObjectsHook != "" {
		args = append(args, "-c")
		args = append(args, fmt.Sprintf("uploadpack.packObjectsHook=%s", r.config.PackObjectsHook))
		env = append(env, fmt.Sprintf("POH_CACHE_DIR=%s", r.config.PackObjectsCache))
		env = append(env, fmt.Sprintf("POH_STDIN_MAX=%d", 1500))
		env = append(env, fmt.Sprintf("POH_CI_SOURCE=%s", ci_source))
	}

	args = append(args, "upload-pack")
	args = append(args, "--stateless-rpc")
	args = append(args, r.localDiskPath)

	cmd := exec.Command(gitBinary, args...)
	cmd.Env = env
	cmd.Dir = r.localDiskPath
	cmd.Stdin = newGitRequest(command)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr

	startTime := time.Now()
	defer logElapsed("serveFetchLocal", startTime, time.Minute, r.localDiskPath)

	atomic.AddInt32(&serveFetchLocalCounter, 1)
	defer atomic.AddInt32(&serveFetchLocalCounter, -1)

	err := cmd.Run()
	elapsed := time.Since(startTime)
	counter := atomic.LoadInt32(&serveFetchLocalCounter)
	StatsdClient.Gauge("goblet.operation.concurrency", float64(counter), []string{"dir:" + r.localDiskPath, "op:servefetchlocal"}, 1)

	if err != nil {
		log.Printf("ServeFetchLocal failed (run:%s, err:%v, counter:%d, dir:%s)\n", elapsed, err, counter, r.localDiskPath)
	} else {
		log.Printf("ServeFetchLocal succeeded (run:%s, counter:%d, dir:%s)\n", elapsed, counter, r.localDiskPath)
	}
	return err
}

func (r *managedRepository) startOperation(op string) RunningOperation {
	if r.config.LongRunningOperationLogger != nil {
		return r.config.LongRunningOperationLogger(op, r.upstreamURL)
	}
	return noopOperation{}
}

func lookupReference(repo *git.Repository, refName string, resolve bool) (*git.Reference, error) {
	if valid, _ := git.ReferenceNameIsValid(refName); !valid {
		log.Printf("Searching ref and got invalid ref %s\n", refName)
		return nil, ErrReferenceInvalid
	}

	ref, err := repo.References.Lookup(refName)
	if err != nil {
		log.Printf("Searching ref and not found %s %v\n", refName, err)
		return nil, ErrReferenceNotFound
	}

	if resolve {
		ref, err = ref.Resolve()
		if err != nil {
			log.Printf("Searching ref and not resolved %s %v\n", refName, err)
			return nil, ErrReferenceNotFound
		}
	}

	return ref, nil
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
	log.Printf("(runGitWithStdOut) Running git %s on %s\n", strings.Join(arg, " "), gitDir)
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
