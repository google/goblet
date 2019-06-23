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
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/goblet"
	"google.golang.org/api/iterator"
)

const (
	gobletRepoManifestDir = "goblet-repository-manifests"

	manifestCleanUpDuration = 24 * time.Hour

	backupFrequency = time.Hour
)

func RunBackupProcess(config *goblet.ServerConfig, bh *storage.BucketHandle, manifestName string, logger *log.Logger) {
	rw := &backupReaderWriter{
		bucketHandle: bh,
		manifestName: manifestName,
		config:       config,
		logger:       logger,
	}
	rw.recoverFromBackup()
	go func() {
		timer := time.NewTimer(backupFrequency)
		for {
			select {
			case <-timer.C:
				rw.saveBackup()
			}
			timer.Reset(backupFrequency)
		}
	}()
}

type backupReaderWriter struct {
	bucketHandle *storage.BucketHandle
	manifestName string
	config       *goblet.ServerConfig
	logger       *log.Logger
}

func (b *backupReaderWriter) recoverFromBackup() {
	repos := b.readRepoList()
	if repos == nil || len(repos) == 0 {
		b.logger.Print("No repositories found from backup")
		return
	}

	for rawURL, _ := range repos {
		u, err := url.Parse(rawURL)
		if err != nil {
			b.logger.Printf("Cannot parse %s as a URL. Skipping", rawURL)
			continue
		}

		bundlePath, err := b.downloadBackupBundle(path.Join(u.Host, u.Path))
		if err != nil {
			b.logger.Printf("Cannot find the backup bundle for %s. Skipping: %v", rawURL, err)
			continue
		}

		m, err := goblet.OpenManagedRepository(b.config, u)
		if err != nil {
			b.logger.Printf("Cannot open a managed repository for %s. Skipping: %v", rawURL, err)
			continue
		}

		m.RecoverFromBundle(bundlePath)
		os.Remove(bundlePath)
	}
}

func (b *backupReaderWriter) readRepoList() map[string]bool {
	it := b.bucketHandle.Objects(context.Background(), &storage.Query{
		Delimiter: "/",
		Prefix:    path.Join(gobletRepoManifestDir, b.manifestName) + "/",
	})
	repos := map[string]bool{}
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			b.logger.Printf("Error while finding the manifests: %v", err)
			return nil
		}
		if attrs.Name == "" {
			continue
		}

		b.readManifest(attrs.Name, repos)
	}
	return repos
}

func (b *backupReaderWriter) readManifest(name string, m map[string]bool) {
	rc, err := b.bucketHandle.Object(name).NewReader(context.Background())
	if err != nil {
		b.logger.Printf("Cannot open a manifest file %s. Skipping: %v", name, err)
		return
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		m[strings.TrimSpace(sc.Text())] = true
	}
	if err := sc.Err(); err != nil {
		b.logger.Printf("Error while reading a manifest file %s. Skipping the rest of the file: %v", name, err)
	}
}

func (b *backupReaderWriter) downloadBackupBundle(name string) (string, error) {
	_, name, err := b.gcBundle(name)
	if name == "" {
		return "", fmt.Errorf("cannot find the bundle for %s: %v", name, err)
	}

	rc, err := b.bucketHandle.Object(name).NewReader(context.Background())
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tmpBundlePath := filepath.Join(b.config.LocalDiskCacheRoot, "tmp-bundle")
	fi, err := os.OpenFile(tmpBundlePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer fi.Close()

	if _, err := io.Copy(fi, rc); err != nil {
		return "", err
	}
	return tmpBundlePath, nil
}

func (b *backupReaderWriter) saveBackup() {
	urls := []string{}
	goblet.ListManagedRepositories(func(m goblet.ManagedRepository) {
		u := m.UpstreamURL()
		latestBundleSecPrecision, _, err := b.gcBundle(path.Join(u.Host, u.Path))
		if err != nil {
			b.logger.Printf("cannot GC bundles for %s. Skipping: %v", u.String(), err)
			return
		}
		// The bundle timestmap is seconds precision.
		if latestBundleSecPrecision.Unix() >= m.LastUpdateTime().Unix() {
			b.logger.Printf("existing bundle for %s is up-to-date %s", u.String(), latestBundleSecPrecision.Format(time.RFC3339))
		} else if err := b.backupManagedRepo(m); err != nil {
			b.logger.Printf("cannot make a backup for %s. Skipping: %v", u.String(), err)
			return
		}

		urls = append(urls, u.String())
	})

	now := time.Now()
	manifestFile := path.Join(gobletRepoManifestDir, b.manifestName, fmt.Sprintf("%012d", now.Unix()))
	if err := b.writeManifestFile(manifestFile, urls); err != nil {
		b.logger.Printf("cannot create %s: %v", manifestFile, err)
		return
	}

	b.garbageCollectOldManifests(now)
}

func (b *backupReaderWriter) gcBundle(name string) (time.Time, string, error) {
	names := []string{}
	it := b.bucketHandle.Objects(context.Background(), &storage.Query{
		Delimiter: "/",
		Prefix:    name + "/",
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return time.Time{}, "", fmt.Errorf("error while finding the bundles to GC: %v", err)
		}
		if attrs.Name == "" {
			continue
		}

		names = append(names, attrs.Name)
	}

	bundles := []string{}
	for _, name := range names {
		// Ignore non-bundles.
		if _, err := strconv.ParseInt(path.Base(names[0]), 10, 64); err != nil {
			continue
		}
		bundles = append(bundles, name)
	}

	if len(bundles) == 0 {
		// No backup found.
		return time.Time{}, "", nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(bundles)))

	for _, name := range bundles[1:len(bundles)] {
		b.bucketHandle.Object(name).Delete(context.Background())
	}
	n, _ := strconv.ParseInt(path.Base(bundles[0]), 10, 64)
	return time.Unix(n, 0), bundles[0], nil
}

func (b *backupReaderWriter) backupManagedRepo(m goblet.ManagedRepository) error {
	u := m.UpstreamURL()
	bundleFile := path.Join(u.Host, u.Path, fmt.Sprintf("%012d", m.LastUpdateTime().Unix()))

	ctx, cf := context.WithCancel(context.Background())
	defer cf()

	wc := b.bucketHandle.Object(bundleFile).NewWriter(ctx)
	if err := m.WriteBundle(wc); err != nil {
		return err
	}
	// Closing here will commit the file. Otherwise, the cancelled context
	// will discard the file.
	wc.Close()
	return nil
}

func (b *backupReaderWriter) writeManifestFile(manifestFile string, urls []string) error {
	ctx, cf := context.WithCancel(context.Background())
	defer cf()

	wc := b.bucketHandle.Object(manifestFile).NewWriter(ctx)
	for _, url := range urls {
		if _, err := io.WriteString(wc, url+"\n"); err != nil {
			return err
		}
	}
	// Closing here will commit the file. Otherwise, the cancelled context
	// will discard the file.
	wc.Close()
	return nil
}

func (b *backupReaderWriter) garbageCollectOldManifests(now time.Time) {
	threshold := now.Add(-manifestCleanUpDuration)
	it := b.bucketHandle.Objects(context.Background(), &storage.Query{
		Delimiter: "/",
		Prefix:    path.Join(gobletRepoManifestDir, b.manifestName) + "/",
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			b.logger.Printf("Error while finding the manifests to GC: %v", err)
			return
		}
		if attrs.Prefix != "" {
			continue
		}

		sec, err := strconv.ParseInt(path.Base(attrs.Name), 10, 64)
		if err != nil {
			continue
		}
		t := time.Unix(sec, 0)
		if t.Before(threshold) {
			b.bucketHandle.Object(attrs.Name).Delete(context.Background())
		}
	}
}
