// +build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cresta/gitdb/internal/gitdb"

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
			Repos:           "git@github.com:cresta/gitdb-reference.git",
			GithubPushToken: "abc123",
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
	t.Run("refresh_repo", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/file/gitdb-reference/master/on_master.txt", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "true\n", requiredRead(t, resp.Body))
	})
	t.Run("not_found_file", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/file/gitdb-reference/master/not_there.txt", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("ls_dir_root", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var bodyResp []gitdb.FileStat
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&bodyResp))
		require.Len(t, bodyResp, 3)
		require.Equal(t, bodyResp[1].Name, "adir")
	})
	t.Run("ls_dir_adir", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/adir", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var bodyResp []gitdb.FileStat
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&bodyResp))
		require.Len(t, bodyResp, 2)
		require.Equal(t, bodyResp[0].Name, "file_in_directory.txt")
	})
	t.Run("ls_dir_missing", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/ls/gitdb-reference/master/missing", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("not_found", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/unknown", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("github_event_invalid_body", func(t *testing.T) {
		resp, err := http.Post(fmt.Sprintf("http://localhost:%d/public/github/push_event", sendPort), "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	require.NoError(t, s.server.Shutdown(context.Background()))
	wg.Wait()
}

func requiredRead(t *testing.T, r io.Reader) string {
	b, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}
