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
	"io"
	"net/http"
	"net/url"
	"time"

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

	URLCanonializer func(*url.URL) (*url.URL, error)

	RequestAuthorizer func(*http.Request) error

	TokenSource func(upstreamURL *url.URL) (*oauth2.Token, error)

	ErrorReporter func(*http.Request, error)

	RequestLogger func(r *http.Request, status int, requestSize, responseSize int64, latency time.Duration)

	LongRunningOperationLogger func(string, *url.URL) RunningOperation
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

func OpenManagedRepository(config *ServerConfig, u *url.URL) (ManagedRepository, error) {
	return openManagedRepository(config, u)
}

func ListManagedRepositories(fn func(ManagedRepository)) {
	managedRepos.Range(func(key, value interface{}) bool {
		m := value.(*managedRepository)
		fn(m)
		return true
	})
}
