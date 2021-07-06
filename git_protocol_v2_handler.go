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
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/gitprotocolio"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	checkFrequency = 1 * time.Second
)

type gitProtocolErrorReporter interface {
	reportError(context.Context, time.Time, error)
}

func handleV2Command(ctx context.Context, reporter gitProtocolErrorReporter, repo *managedRepository, command []*gitprotocolio.ProtocolV2RequestChunk, w io.Writer) bool {
	startTime := time.Now()
	var err error
	ctx, err = tag.New(ctx, tag.Upsert(CommandTypeKey, command[0].Command))
	if err != nil {
		reporter.reportError(ctx, startTime, err)
		return false
	}

	cacheState := "locally-served"
	ctx, err = tag.New(ctx, tag.Upsert(CommandCacheStateKey, cacheState))
	if err != nil {
		reporter.reportError(ctx, startTime, err)
		return false
	}
	switch command[0].Command {
	case "ls-refs":
		ctx, err = tag.New(ctx, tag.Update(CommandCacheStateKey, "queried-upstream"))
		if err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		}

		resp, err := repo.lsRefsUpstream(command)
		if err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		}

		refs, err := parseLsRefsResponse(resp)
		if err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		}

		if hasUpdate, err := repo.hasAnyUpdate(refs); err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		} else if hasUpdate {
			go repo.fetchUpstream()
		}

		writeResp(w, resp)
		reporter.reportError(ctx, startTime, nil)
		return true

	case "fetch":
		wantHashes, wantRefs, err := parseFetchWants(command)
		if err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		}

		if hasAllWants, err := repo.hasAllWants(wantHashes, wantRefs); err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		} else if !hasAllWants {
			ctx, err = tag.New(ctx, tag.Update(CommandCacheStateKey, "queried-upsteam"))
			if err != nil {
				reporter.reportError(ctx, startTime, err)
				return false
			}

			fetchStartTime := time.Now()
			fetchDone := make(chan error, 1)
			go func() {
				fetchDone <- repo.fetchUpstream()
			}()
			timer := time.NewTimer(checkFrequency)
		LOOP:
			for {
				select {
				case <-ctx.Done():
					reporter.reportError(ctx, startTime, ctx.Err())
					return false
				case err := <-fetchDone:
					if hasAllWants, checkErr := repo.hasAllWants(wantHashes, wantRefs); checkErr != nil {
						reporter.reportError(ctx, startTime, checkErr)
						return false
					} else if !hasAllWants {
						reporter.reportError(ctx, startTime, err)
						return false
					}
					break LOOP
				case <-timer.C:
					if hasAllWants, err := repo.hasAllWants(wantHashes, wantRefs); err != nil {
						reporter.reportError(ctx, startTime, err)
						return false
					} else if hasAllWants {
						break LOOP
					}
					timer.Reset(checkFrequency)
				}
			}
			stats.Record(ctx, UpstreamFetchWaitingTime.M(int64(time.Now().Sub(fetchStartTime)/time.Millisecond)))
		}

		if err := repo.serveFetchLocal(command, w); err != nil {
			reporter.reportError(ctx, startTime, err)
			return false
		}
		reporter.reportError(ctx, startTime, nil)
		return true
	}
	reporter.reportError(ctx, startTime, status.Error(codes.InvalidArgument, "unknown command"))
	return false
}

func parseLsRefsResponse(chunks []*gitprotocolio.ProtocolV2ResponseChunk) (map[string]plumbing.Hash, error) {
	m := map[string]plumbing.Hash{}
	for _, ch := range chunks {
		if ch.Response == nil {
			continue
		}
		ss := strings.Split(string(ch.Response), " ")
		if len(ss) < 2 {
			return nil, status.Errorf(codes.Internal, "cannot parse the upstream ls-refs response: got %d component, want at least 2", len(ss))
		}
		m[strings.TrimSpace(ss[1])] = plumbing.NewHash(ss[0])
	}
	return m, nil
}

func parseFetchWants(chunks []*gitprotocolio.ProtocolV2RequestChunk) ([]plumbing.Hash, []string, error) {
	hashes := []plumbing.Hash{}
	refs := []string{}
	for _, ch := range chunks {
		if ch.Argument == nil {
			continue
		}
		s := string(ch.Argument)
		if strings.HasPrefix(s, "want ") {
			ss := strings.Split(s, " ")
			if len(ss) < 2 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "cannot parse the fetch request: got %d component, want at least 2", len(ss))
			}
			hashes = append(hashes, plumbing.NewHash(strings.TrimSpace(ss[1])))
		} else if strings.HasPrefix(s, "want-ref ") {
			ss := strings.Split(s, " ")
			if len(ss) < 2 {
				return nil, nil, status.Errorf(codes.InvalidArgument, "cannot parse the fetch request: got %d component, want at least 2", len(ss))
			}
			refs = append(refs, strings.TrimSpace(ss[1]))
		}
	}
	return hashes, refs, nil
}
