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

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	_ "net/http/pprof"
	"net/url"
	"os"
	"time"

	datadog "github.com/DataDog/opencensus-go-exporter-datadog"
	"github.com/canva/goblet"
	"github.com/canva/goblet/github"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

var (
	config      = flag.String("config", "", "Path to Goblet's configuration file")
	checkConfig = flag.Bool("check", false, "Only checking if the config is valid, then exit")

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

func FetchRepositories(config *goblet.ServerConfig, repositories []string, mustFetch bool) []error {
	errorChans := make([]chan error, 0, len(repositories))

	for _, repository := range repositories {
		errorChan := make(chan error, 1)
		errorChans = append(errorChans, errorChan)
		u, err := url.Parse(repository)
		if err != nil {
			errorChan <- err
		} else {
			goblet.FetchManagedRepositoryAsync(config, u, mustFetch, errorChan)
		}
	}

	errors := make([]error, 0)
	for _, errorChan := range errorChans {
		err := <-errorChan
		if err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}

func main() {
	flag.Parse()

	if *config == "" {
		log.Fatal("The '-config' argument is mandatory")
	}

	configFile, err := goblet.LoadConfigFile(*config)
	if err != nil {
		log.Fatalf("Couldn't load the configuration file: %v\n", err)
	}

	if *checkConfig {
		fmt.Println("Config is valid")
		return
	}

	if err := view.Register(views...); err != nil {
		log.Fatal(err)
	}

	var er = func(r *http.Request, err error) {
		log.Printf("Error while processing a request: %v", err)
	}

	var rl = func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration) {
		_, err := httputil.DumpRequest(r, false)
		if err != nil {
			return
		}
		// log.Printf("%q %d reqsize: %d, respsize %d, latency: %v", dump, status, requestSize, responseSize, latency)
	}

	var lrol = func(action string, u *url.URL) goblet.RunningOperation {
		log.Printf("Starting %s for %s", action, u.String())
		return &logBasedOperation{action, u}
	}

	ts, err := github.NewTokenSource(
		os.Getenv("GH_APP_ID"),
		os.Getenv("GH_APP_INSTALLATION_ID"),
		os.Getenv("GH_APP_PRIVATE_KEY"),
		time.Duration(configFile.TokenExpiryDeltaSeconds)*time.Second,
	)

	if err != nil {
		log.Fatal(err)
	}

	authorizer := github.NewAuthorizer(true, goblet.StatsdClient)
	defer authorizer.Close()

	config := &goblet.ServerConfig{
		LocalDiskCacheRoot:         configFile.CacheRoot,
		URLCanonicalizer:           github.URLCanonicalizer,
		RequestAuthorizer:          goblet.NoOpRequestAuthorizer,
		TokenSource:                ts,
		ErrorReporter:              er,
		RequestLogger:              rl,
		LongRunningOperationLogger: lrol,
		PackObjectsHook:            configFile.PackObjectsHook,
		PackObjectsCache:           configFile.PackObjectsCache,
	}

	if configFile.EnableMetrics {
		log.Println("Initializing Datadog exporter...")
		dd, err := datadog.NewExporter(datadog.Options{})
		if err != nil {
			log.Fatalf("Failed to create the Datadog exporter: %v", err)
		}
		// It is imperative to invoke flush before your main function exits
		defer dd.Stop()

		view.RegisterExporter(dd)
	}

	if configFile.PackObjectsHook != "" {
		if configFile.PackObjectsCache == "" {
			log.Fatalf("pack_objects_cache must be set in config, if pack_objects_hook is set.")
		}
	}

	log.Println("Initializing repositories...")
	for _, repository := range configFile.Repositories {
		u, err := url.Parse(repository)
		if err != nil {
			log.Fatalf("Failed to initialize repository '%s': %v", repository, err)
		}

		_, err = goblet.OpenManagedRepository(config, u)
		if err != nil {
			log.Fatalf("Failed to initialize repository '%s': %v", repository, err)
		}
	}

	// Pre-fetch repositories before serving any traffic. This prevents initial
	// requests from being blocked a long time until the repositories cache is
	// ready.
	log.Println("Pre-fetching repositories...")
	if errs := FetchRepositories(config, configFile.Repositories, true); len(errs) > 0 {
		for _, err := range errs {
			log.Println(err)
		}
		os.Exit(1)
	}

	// Schedule periodic upstream fetches every 15 minutes.
	log.Println("Starting background fetches...")
	cancel := goblet.RunEvery(15*time.Minute, func(t time.Time) {
		for _, err := range FetchRepositories(config, configFile.Repositories, false) {
			log.Println(err)
		}
	})
	defer cancel()

	// Report disk usage metrics every 1 minute
	log.Println("Starting disk usage monitoring...")
	cancelDiskMonitoring := goblet.RunEvery(1*time.Minute, func(t time.Time) {
		if err := goblet.ReportDiskUsage(configFile.CacheRoot); err != nil {
			log.Printf("Failed to report disk usage: %v\n", err)
		}
	})
	defer cancelDiskMonitoring()

	log.Println("Registering HTTP routes...")
	http.Handle("/", goblet.HTTPHandler(config))

	http.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok\n")
	})

	http.HandleFunc("/authcache", authorizer.CacheMetricsHandler)

	log.Printf("Starting HTTP server on port %d...\n", configFile.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", configFile.Port), nil))
}

type logBasedOperation struct {
	action string
	u      *url.URL
}

func (op *logBasedOperation) Printf(format string, a ...interface{}) {
	// log.Printf("Progress %s (%s): %s", op.action, op.u.String(), fmt.Sprintf(format, a...))
}

func (op *logBasedOperation) Done(err error) {
	log.Printf("Finished %s for %s: %v", op.action, op.u.String(), err)
}
