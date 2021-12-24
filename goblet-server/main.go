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

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"cloud.google.com/go/errorreporting"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/storage"
	"contrib.go.opencensus.io/exporter/stackdriver"
	"github.com/google/goblet"
	googlehook "github.com/google/goblet/google"
	"github.com/google/uuid"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	logpb "google.golang.org/genproto/googleapis/logging/v2"
)

const (
	scopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
	scopeUserInfoEmail = "https://www.googleapis.com/auth/userinfo.email"
)

var (
	port      = flag.Int("port", 8080, "port to listen to")
	cacheRoot = flag.String("cache_root", "", "Root directory of cached repositories")

	stackdriverProject      = flag.String("stackdriver_project", "", "GCP project ID used for the Stackdriver integration")
	stackdriverLoggingLogID = flag.String("stackdriver_logging_log_id", "", "Stackdriver logging Log ID")

	backupBucketName   = flag.String("backup_bucket_name", "", "Name of the GCS bucket for backed-up repositories")
	backupManifestName = flag.String("backup_manifest_name", "", "Name of the backup manifest")

	latencyDistributionAggregation = view.Distribution(
		100,
		200,
		400,
		800,
		1000, // 1s
		2000,
		4000,
		8000,
		10000, // 10s
		20000,
		40000,
		80000,
		100000, // 100s
		200000,
		400000,
		800000,
		1000000, // 1000s
		2000000,
		4000000,
		8000000,
	)
	views = []*view.View{
		{
			Name:        "github.com/google/goblet/inbound-command-count",
			Description: "Inbound command count",
			TagKeys:     []tag.Key{goblet.CommandTypeKey, goblet.CommandCanonicalStatusKey, goblet.CommandCacheStateKey},
			Measure:     goblet.InboundCommandCount,
			Aggregation: view.Count(),
		},
		{
			Name:        "github.com/google/goblet/inbound-command-latency",
			Description: "Inbound command latency",
			TagKeys:     []tag.Key{goblet.CommandTypeKey, goblet.CommandCanonicalStatusKey, goblet.CommandCacheStateKey},
			Measure:     goblet.InboundCommandProcessingTime,
			Aggregation: latencyDistributionAggregation,
		},
		{
			Name:        "github.com/google/goblet/outbound-command-count",
			Description: "Outbound command count",
			TagKeys:     []tag.Key{goblet.CommandTypeKey, goblet.CommandCanonicalStatusKey},
			Measure:     goblet.OutboundCommandCount,
			Aggregation: view.Count(),
		},
		{
			Name:        "github.com/google/goblet/outbound-command-latency",
			Description: "Outbound command latency",
			TagKeys:     []tag.Key{goblet.CommandTypeKey, goblet.CommandCanonicalStatusKey},
			Measure:     goblet.OutboundCommandProcessingTime,
			Aggregation: latencyDistributionAggregation,
		},
		{
			Name:        "github.com/google/goblet/upstream-fetch-blocking-time",
			Description: "Duration that requests are waiting for git-fetch from the upstream",
			Measure:     goblet.UpstreamFetchWaitingTime,
			Aggregation: latencyDistributionAggregation,
		},
	}
)

func main() {
	flag.Parse()

	ts, err := google.DefaultTokenSource(context.Background(), scopeCloudPlatform, scopeUserInfoEmail)
	if err != nil {
		log.Fatalf("Cannot initialize the OAuth2 token source: %v", err)
	}
	authorizer, err := googlehook.NewRequestAuthorizer(ts)
	if err != nil {
		log.Fatalf("Cannot create a request authorizer: %v", err)
	}
	if err := view.Register(views...); err != nil {
		log.Fatal(err)
	}

	var er func(*http.Request, error)
	var rl func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration) = func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration) {
		dump, err := httputil.DumpRequest(r, false)
		if err != nil {
			return
		}
		log.Printf("%q %d reqsize: %d, respsize %d, latency: %v", dump, status, requestSize, responseSize, latency)
	}
	var lrol func(string, *url.URL) goblet.RunningOperation = func(action string, u *url.URL) goblet.RunningOperation {
		log.Printf("Starting %s for %s", action, u.String())
		return &logBasedOperation{action, u}
	}
	var backupLogger *log.Logger = log.New(os.Stderr, "", log.LstdFlags)
	if *stackdriverProject != "" {
		// Error reporter
		ec, err := errorreporting.NewClient(context.Background(), *stackdriverProject, errorreporting.Config{
			ServiceName: "goblet",
		})
		if err != nil {
			log.Fatalf("Cannot create a Stackdriver errorreporting client: %v", err)
		}
		defer func() {
			if err := ec.Close(); err != nil {
				log.Printf("Failed to report errors to Stackdriver: %v", err)
			}
		}()
		er = func(r *http.Request, err error) {
			ec.Report(errorreporting.Entry{
				Req:   r,
				Error: err,
			})
			log.Printf("Error while processing a request: %v", err)
		}

		if *stackdriverLoggingLogID != "" {
			lc, err := logging.NewClient(context.Background(), *stackdriverProject)
			if err != nil {
				log.Fatalf("Cannot create a Stackdriver logging client: %v", err)
			}
			defer func() {
				if err := lc.Close(); err != nil {
					log.Printf("Failed to log requests to Stackdriver: %v", err)
				}
			}()

			// Request logger
			sdLogger := lc.Logger(*stackdriverLoggingLogID)
			rl = func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration) {
				sdLogger.Log(logging.Entry{
					HTTPRequest: &logging.HTTPRequest{
						Request:      r,
						RequestSize:  requestSize,
						Status:       status,
						ResponseSize: responseSize,
						Latency:      latency,
						RemoteIP:     r.RemoteAddr,
					},
				})
			}
			lrol = func(action string, u *url.URL) goblet.RunningOperation {
				op := &stackdriverBasedOperation{
					sdLogger:  sdLogger,
					action:    action,
					u:         u,
					startTime: time.Now(),
					id:        uuid.New().String(),
				}
				op.sdLogger.Log(logging.Entry{
					Payload: &LongRunningOperation{
						Action: op.action,
						URL:    op.u.String(),
					},
					Operation: &logpb.LogEntryOperation{
						Id:       op.id,
						Producer: "github.com/google/goblet",
						First:    true,
					},
				})
				return op
			}
			// Backup logger
			backupLogger = sdLogger.StandardLogger(logging.Warning)
		}

		// OpenCensus view exporters.
		exporter, err := stackdriver.NewExporter(stackdriver.Options{
			ProjectID: *stackdriverProject,
		})
		if err != nil {
			log.Fatal(err)
		}
		if err = exporter.StartMetricsExporter(); err != nil {
			log.Fatal(err)
		}
	}

	config := &goblet.ServerConfig{
		LocalDiskCacheRoot:         *cacheRoot,
		URLCanonializer:            googlehook.CanonicalizeURL,
		RequestAuthorizer:          authorizer,
		TokenSource:                func(upstreamURL *url.URL) (*oauth2.Token, error) {
			return ts.Token()
		},
		ErrorReporter:              er,
		RequestLogger:              rl,
		LongRunningOperationLogger: lrol,
	}

	if *backupBucketName != "" && *backupManifestName != "" {
		gsClient, err := storage.NewClient(context.Background())
		if err != nil {
			log.Fatal(err)
		}

		googlehook.RunBackupProcess(config, gsClient.Bucket(*backupBucketName), *backupManifestName, backupLogger)
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok\n")
	})
	http.Handle("/", goblet.HTTPHandler(config))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

type LongRunningOperation struct {
	Action          string `json:"action"`
	URL             string `json:"url"`
	DurationMs      int    `json:"duration_msec,omitempty"`
	Error           string `json:"error,omitempty"`
	ProgressMessage string `json:"progress_message,omitempty"`
}

type logBasedOperation struct {
	action string
	u      *url.URL
}

func (op *logBasedOperation) Printf(format string, a ...interface{}) {
	log.Printf("Progress %s (%s): %s", op.action, op.u.String(), fmt.Sprintf(format, a...))
}

func (op *logBasedOperation) Done(err error) {
	log.Printf("Finished %s for %s: %v", op.action, op.u.String(), err)
}

type stackdriverBasedOperation struct {
	sdLogger  *logging.Logger
	action    string
	u         *url.URL
	startTime time.Time
	id        string
}

func (op *stackdriverBasedOperation) Printf(format string, a ...interface{}) {
	lro := &LongRunningOperation{
		Action:          op.action,
		URL:             op.u.String(),
		ProgressMessage: fmt.Sprintf(format, a...),
	}
	op.sdLogger.Log(logging.Entry{
		Payload: lro,
		Operation: &logpb.LogEntryOperation{
			Id:       op.id,
			Producer: "github.com/google/goblet",
		},
	})
}

func (op *stackdriverBasedOperation) Done(err error) {
	lro := &LongRunningOperation{
		Action:     op.action,
		URL:        op.u.String(),
		DurationMs: int(time.Since(op.startTime) / time.Millisecond),
	}
	if err != nil {
		lro.Error = err.Error()
	}
	op.sdLogger.Log(logging.Entry{
		Payload: lro,
		Operation: &logpb.LogEntryOperation{
			Id:       op.id,
			Producer: "github.com/google/goblet",
			Last:     true,
		},
	})
}
