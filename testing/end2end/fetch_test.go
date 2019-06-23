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

package end2end

import (
	"testing"

	goblettest "github.com/google/goblet/testing"
)

func TestFetch(t *testing.T) {
	ts := goblettest.NewTestServer(&goblettest.TestServerConfig{
		RequestAuthorizer: goblettest.TestRequestAuthorizer,
		TokenSource:       goblettest.TestTokenSource,
	})
	defer ts.Close()

	want, err := ts.CreateRandomCommitUpstream()
	if err != nil {
		t.Fatal(err)
	}

	client := goblettest.NewLocalGitRepo()
	defer client.Close()
	if _, err := client.Run("-c", "http.extraHeader=Authorization: Bearer "+goblettest.ValidClientAuthToken, "fetch", ts.ProxyServerURL); err != nil {
		t.Fatal(err)
	}

	if got, err := client.Run("rev-parse", "FETCH_HEAD"); err != nil {
		t.Error(err)
	} else if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestFetch_ForceFetchUpdate(t *testing.T) {
	ts := goblettest.NewTestServer(&goblettest.TestServerConfig{
		RequestAuthorizer: goblettest.TestRequestAuthorizer,
		TokenSource:       goblettest.TestTokenSource,
	})
	defer ts.Close()

	_, err := ts.CreateRandomCommitUpstream()
	if err != nil {
		t.Fatal(err)
	}

	client := goblettest.NewLocalGitRepo()
	defer client.Close()
	if _, err := client.Run("remote", "add", "origin", ts.ProxyServerURL); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run("-c", "http.extraHeader=Authorization: Bearer "+goblettest.ValidClientAuthToken, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}

	want, err := ts.CreateRandomCommitUpstream()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Run("-c", "http.extraHeader=Authorization: Bearer "+goblettest.ValidClientAuthToken, "fetch", "origin", "master"); err != nil {
		t.Fatal(err)
	}

	if got, err := client.Run("rev-parse", "FETCH_HEAD"); err != nil {
		t.Error(err)
	} else if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
