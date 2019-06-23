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

package google

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	oauth2cli "google.golang.org/api/oauth2/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	scopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
	scopeUserInfoEmail = "https://www.googleapis.com/auth/userinfo.email"
)

// NewRequestAuthorizer returns a function that checks the authorization header
// and authorize the request.
func NewRequestAuthorizer(ts oauth2.TokenSource) (func(*http.Request) error, error) {
	// Restrict the access to the proxy to the same user as the server's
	// service account. This makes sure that the server won't expose the
	// contents that the proxy clients cannot access, and the access
	// auditing is done properly.

	oauth2Service, err := oauth2cli.NewService(context.Background(), option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("cannot initialize the OAuth2 service: %v", err)
	}

	// Get the server's service account.
	t, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("cannot obtain an OAuth2 access token for the server: %v", err)
	}
	c := oauth2Service.Tokeninfo()
	c.AccessToken(t.AccessToken)
	ti, err := c.Do()
	if err != nil {
		return nil, fmt.Errorf("failed to call OAuth2 TokenInfo: %v", err)
	}

	// Check that the server setup is correct.
	hasCloudPlatform, hasUserInfoEmail := scopeCheck(ti.Scope)
	if !hasCloudPlatform {
		return nil, fmt.Errorf("the server credential doesn't have %s scope. This is needed to access upstream repositories.", scopeCloudPlatform)
	}
	if !hasUserInfoEmail {
		return nil, fmt.Errorf("the server credential doesn't have %s scope. This is needed to get the email address of the service account.", scopeUserInfoEmail)
	}
	if ti.Email == "" {
		return nil, fmt.Errorf("cannot obtain the server's service account email")
	}

	email := ti.Email
	return func(r *http.Request) error {
		if h := r.Header.Get("Authorization"); h != "" {
			return authorizeAuthzHeader(oauth2Service, email, h)
		}
		if c, err := r.Cookie("o"); err == nil {
			return authorizeCookie(oauth2Service, email, c.Value)
		}
		return status.Error(codes.Unauthenticated, "no auth token")
	}, nil
}

func authorizeAuthzHeader(oauth2Service *oauth2cli.Service, email, authorizationHeader string) error {
	accessToken := ""
	if strings.HasPrefix(authorizationHeader, "Bearer ") {
		accessToken = strings.TrimPrefix(authorizationHeader, "Bearer ")
	} else if strings.HasPrefix(authorizationHeader, "Basic ") {
		bs, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorizationHeader, "Basic "))
		if err != nil {
			return status.Error(codes.Unauthenticated, "cannot parse the Authorization header")
		}
		s := string(bs)
		i := strings.IndexByte(s, ':')
		if i < 0 {
			return status.Error(codes.Unauthenticated, "cannot parse the Authorization header")
		}
		accessToken = s[i+1:]
	} else {
		return status.Error(codes.Unauthenticated, "no bearer token")
	}
	return authorizeAccessToken(oauth2Service, email, accessToken)
}

func authorizeCookie(oauth2Service *oauth2cli.Service, email, oCookie string) error {
	if strings.ContainsRune(oCookie, '=') {
		oCookie = strings.SplitN(oCookie, "=", 2)[1]
	}
	return authorizeAccessToken(oauth2Service, email, oCookie)
}

func authorizeAccessToken(oauth2Service *oauth2cli.Service, email, accessToken string) error {
	c := oauth2Service.Tokeninfo()
	c.AccessToken(accessToken)
	ti, err := c.Do()
	if err != nil {
		return status.Errorf(codes.Unavailable, "cannot call OAuth2 TokenInfo: %v", err)
	}

	hasCloudPlatform, hasUserInfoEmail := scopeCheck(ti.Scope)
	if !hasCloudPlatform {
		return status.Errorf(codes.Unauthenticated, "access token doesn't have %s", scopeCloudPlatform)
	}
	if !hasUserInfoEmail {
		return status.Errorf(codes.Unauthenticated, "access token doesn't have %s", scopeUserInfoEmail)
	}

	if ti.Email != email {
		// Do not send the server's service account email so that a
		// stranger cannot know the server's service account. The proxy
		// server should be running in a private network, but this is
		// an extra protection.
		return status.Errorf(codes.Unauthenticated, "access token attests a different user %s", ti.Email)
	}

	return nil
}

// CanonicalizeURL returns a canonicalized URL for googlesource.com and source.developers.google.com.
func CanonicalizeURL(u *url.URL) (*url.URL, error) {
	ret := url.URL{}
	ret.Scheme = "https"
	ret.Host = u.Host
	ret.Path = u.Path

	if strings.HasSuffix(ret.Host, ".googlesource.com") {
		if strings.HasPrefix(ret.Path, "/a/") {
			// Force authorization prefix.
			ret.Path = strings.TrimPrefix(ret.Path, "/a")
		}
	} else if ret.Host == "source.developers.google.com" {
		// Do nothing.
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported host: %s", u.Host)
	}
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

func scopeCheck(scopes string) (bool, bool) {
	hasCloudPlatform := false
	hasUserInfoEmail := false
	for _, scope := range strings.Split(scopes, " ") {
		if scope == scopeCloudPlatform {
			hasCloudPlatform = true
		}
		if scope == scopeUserInfoEmail {
			hasUserInfoEmail = true
		}
	}
	return hasCloudPlatform, hasUserInfoEmail
}
