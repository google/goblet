// Copyright 2021 Canva Inc
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
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/oauth2"
)

var (
	// CommandTypeKey indicates a command type ("ls-refs", "fetch",
	// "not-a-command").
	CommandTypeKey = tag.MustNewKey("github.com/google/goblet/command-type")

	// CommandCacheStateKey indicates whether the command response is cached
	// or not ("locally-served", "queried-upstream").
	CommandCacheStateKey = tag.MustNewKey("github.com/google/goblet/command-cache-state")

	// CommandCanonicalStatusKey indicates whether the command is succeeded
	// or not ("OK", "Unauthenticated").
	CommandCanonicalStatusKey = tag.MustNewKey("github.com/google/goblet/command-status")

	// InboundCommandProcessingTime is a processing time of the inbound
	// commands.
	InboundCommandProcessingTime = stats.Int64("github.com/google/goblet/inbound-command-processing-time", "processing time of inbound commands", stats.UnitMilliseconds)

	// OutboundCommandProcessingTime is a processing time of the outbound
	// commands.
	OutboundCommandProcessingTime = stats.Int64("github.com/google/goblet/outbound-command-processing-time", "processing time of outbound commands", stats.UnitMilliseconds)

	// UpstreamFetchWaitingTime is a duration that a fetch request waited
	// for the upstream.
	UpstreamFetchWaitingTime = stats.Int64("github.com/google/goblet/upstream-fetch-waiting-time", "waiting time of upstream fetch command", stats.UnitMilliseconds)

	// InboundCommandCount is a count of inbound commands.
	InboundCommandCount = stats.Int64("github.com/google/goblet/inbound-command-count", "number of inbound commands", stats.UnitDimensionless)

	// OutboundCommandCount is a count of outbound commands.
	OutboundCommandCount = stats.Int64("github.com/google/goblet/outbound-command-count", "number of outbound commands", stats.UnitDimensionless)
)

type ServerConfig struct {
	LocalDiskCacheRoot string

	URLCanonicalizer func(*url.URL) (*url.URL, error)

	RequestAuthorizer func(*http.Request) error

	TokenSource oauth2.TokenSource

	ErrorReporter func(*http.Request, error)

	RequestLogger func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration)

	LongRunningOperationLogger func(string, *url.URL) RunningOperation

	PackObjectsHook string

	PackObjectsCache string
}

type RunningOperation interface {
	Printf(format string, a ...interface{})

	Done(error)
}

type ManagedRepository interface {
	UpstreamURL() *url.URL

	LastUpdateTime() time.Time

	RecoverFromBundle(string) error

	WriteBundle(io.Writer) error
}

func HTTPHandler(config *ServerConfig) http.Handler {
	return &httpProxyServer{config}
}

// RunEvery schedules a given function to be executed on a duty cycle. A cancellation function is
// returned to prevent any future executions. In-flight execution cancellations are delegated to
// callers.
func RunEvery(delay time.Duration, f func(t time.Time)) func() {
	stop := make(chan bool)
	go func() {
		for {
			select {
			case t := <-time.After(delay):
				f(t)
			case <-stop:
				return
			}
		}
	}()

	return func() { stop <- true }
}

func OpenManagedRepository(config *ServerConfig, u *url.URL) (ManagedRepository, error) {
	return openManagedRepository(config, u)
}

func FetchManagedRepositoryAsync(config *ServerConfig, u *url.URL, mustFetch bool, errorChan chan<- error) {
	repo, err := openManagedRepository(config, u)
	if err != nil {
		errorChan <- err
		return
	}

	if !mustFetch {
		pendingFetches := repo.fetchUpstreamPool.WaitingTasks()
		if pendingFetches > 0 {
			log.Printf("FetchManagedRepository skipped since there are %d pending fetches (%s)\n", pendingFetches, repo.localDiskPath)
			errorChan <- nil
			return
		}
		elapsedSinceLastUpdate := time.Since(repo.LastUpdateTime())
		if elapsedSinceLastUpdate < 15*time.Minute {
			log.Printf("FetchManagedRepository skipped since repo was updated %s ago (%s)\n", elapsedSinceLastUpdate, repo.localDiskPath)
			errorChan <- nil
			return
		}
	}

	fetchStartTime := time.Now()
	repo.fetchUpstreamPool.Submit(func() {
		logElapsed("FetchManagedRepository queuing", fetchStartTime, time.Minute, repo.localDiskPath)

		if mustFetch {
			log.Printf("FetchManagedRepository required since mustFetch is set (%s)\n", repo.localDiskPath)
			StatsdClient.Incr("goblet.operation.count", []string{"dir:" + repo.localDiskPath, "op:background_fetch", "must:1"}, 1)
			errorChan <- repo.fetchUpstream(nil)
		} else {
			// check again when the task is picked up
			elapsedSinceLastUpdate := time.Since(repo.LastUpdateTime())
			if elapsedSinceLastUpdate < 15*time.Minute {
				log.Printf("FetchManagedRepository skipped since repo was updated %s ago (%s)\n", elapsedSinceLastUpdate, repo.localDiskPath)
				errorChan <- nil
			} else {
				log.Printf("FetchManagedRepository required since repo was not updated for %s (%s)\n", elapsedSinceLastUpdate, repo.localDiskPath)
				StatsdClient.Incr("goblet.operation.count", []string{"dir:" + repo.localDiskPath, "op:background_fetch", "must:0"}, 1)
				errorChan <- repo.fetchUpstream(nil)
			}
		}

		// log gc.log content if any
		if content, err := os.ReadFile(path.Join(repo.localDiskPath, "gc.log")); err == nil {
			log.Printf("Found git gc log file (content:%s, dir:%s)\n", strings.ReplaceAll(string(content), "\n", " "), repo.localDiskPath)
			log.Print("Running git gc b/c gc.log file was found")
			errorChan <- repo.runGC()
		}
	})
}

// DefaultURLCanonicalizer is a URLCanonicalizer implementation that agnostic to any Git hosting provider.
func DefaultURLCanonicalizer(u *url.URL) (*url.URL, error) {
	ret := url.URL{}
	ret.Scheme = "https"
	ret.Host = strings.ToLower(u.Host)
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
	return &ret, nil
}

// NoOpRequestAuthorizer is a request authorizer that always succeeds and checks nothing
// about the incoming request.
func NoOpRequestAuthorizer(request *http.Request) error {
	return nil
}

func seedRepository(config *ServerConfig, repository *managedRepository) error {
	bucket := os.Getenv("FIGMA_CI_CACHE_BUCKET")
	key := "figma.tar"

	var ctx = context.Background()
	client, err := getS3Client(ctx)
	if err != nil {
		log.Println("Could not get S3 client")
		return err
	}

	// Get the object
	output, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		log.Println("Could not get the object")
		return err
	}
	defer output.Body.Close()

	// Create a gzip reader
	gr, err := gzip.NewReader(output.Body)
	if err != nil {
		log.Println("Could not get the gzip reader")
		return err
	}
	defer gr.Close()

	// Create a tar reader
	tr := tar.NewReader(gr)

	// Iterate through the files in the tar archive
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// End of archive
			break
		}
		if err != nil {
			return err
		}

		// Create the file
		outFile, err := os.Create(filepath.Join(repository.localDiskPath, hdr.Name))
		if err != nil {
			return err
		}

		// Copy the file data to the file
		if _, err := io.Copy(outFile, tr); err != nil {
			outFile.Close()
			return err
		}

		// Close immediately instead of deferring in loop
		if err := outFile.Close(); err != nil {
			return err
		}
	}

	log.Println("Extracted", key)
	return nil
}

func getS3Client(ctx context.Context) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-west-2"))
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg), nil
}
