// +build integration

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cresta/gitdb/internal/gitdb/goget"

	"github.com/cresta/gitdb/internal/testhelp"

	"github.com/stretchr/testify/require"
)

func TestServer(t *testing.T) {
	var atomicOnExit int64
	listenDone := make(chan int)

	s := Service{
		osExit: func(i int) {
			atomic.StoreInt64(&atomicOnExit, int64(i))
		},
		log: testhelp.ZapTestingLogger(t),
		config: config{
			ListenAddr:      ":0",
			DataDirectory:   "",
			GithubPushToken: "abc123",
		},
		repoConfig: &RepoConfig{
			Repositories: []Repository{
				{
					URL: "git@github.com:cresta/gitdb-reference.git",
				},
			},
		},
		onListen: func(l net.Listener) {
			listenDone <- l.(*net.TCPListener).Addr().(*net.TCPAddr).Port
		},
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Main()
	}()
	sendPort := <-listenDone
	t.Run("test_health", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "OK", requiredRead(t, resp.Body))
	})
	t.Run("test_refresh", func(t *testing.T) {
		resp, err := http.Post(fmt.Sprintf("http://localhost:%d/refresh/gitdb-reference", sendPort), "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "OK", requiredRead(t, resp.Body))
	})
	t.Run("fetch_file", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/file/gitdb-reference/master/on_master.txt", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "true\n", requiredRead(t, resp.Body))
	})
	t.Run("zip_dir_missing", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/zip/gitdb-reference/master/baddir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("zip_bad_branch", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/zip/gitdb-reference/badbranch/adir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("zip_dir", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/zip/gitdb-reference/master/adir/subdir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var buf bytes.Buffer
		_, err2 := io.Copy(&buf, resp.Body)
		require.NoError(t, err2)
		r, err := zip.NewReader(strings.NewReader(buf.String()), int64(buf.Len()))
		require.NoError(t, err)
		require.Len(t, r.File, 2)
		contents := make(map[string]string)
		for _, f := range r.File {
			rc, err := f.Open()
			require.NoError(t, err)
			var b bytes.Buffer
			_, err3 := io.Copy(&b, rc)
			require.NoError(t, err3)
			require.NoError(t, rc.Close())
			contents[f.Name] = b.String()
		}
		require.Equal(t, map[string]string{
			"subdir_file.txt":  "file1\n",
			"subdir_file2.txt": "file2\n",
		}, contents)
	})
	t.Run("not_found_file", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/file/gitdb-reference/master/not_there.txt", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("not_found_branch", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/file/gitdb-reference/blarg/README.md", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("ls_dir_root", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var bodyResp []goget.FileStat
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&bodyResp))
		require.Len(t, bodyResp, 3)
		require.Equal(t, bodyResp[1].Name, "adir")
	})
	t.Run("ls_dir_adir", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/adir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var bodyResp []goget.FileStat
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&bodyResp))
		require.Len(t, bodyResp, 2)
		require.Equal(t, bodyResp[0].Name, "file_in_directory.txt")
	})
	t.Run("ls_dir_adir_staging", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/staging/adir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var bodyResp []goget.FileStat
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&bodyResp))
		require.Len(t, bodyResp, 1)
		require.Equal(t, bodyResp[0].Name, "file_in_directory.txt")
	})
	t.Run("ls_dir_missing", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/missing", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("ls_dir_bad_branch", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/blarg/adir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("not_found", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/unknown", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("github_event_invalid_body", func(t *testing.T) {
		resp, err := http.Post(fmt.Sprintf("http://localhost:%d/public/github/webhook", sendPort), "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	require.NoError(t, s.server.Shutdown(context.Background()))
	wg.Wait()
}

func requiredRead(t *testing.T, r io.Reader) string {
	b, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}
