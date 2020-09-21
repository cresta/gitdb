package main

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

func TestServer(t *testing.T) {
	var atomicOnExit int64
	listenDone := make(chan int)
	s := Service{
		osExit: func(i int) {
			atomic.StoreInt64(&atomicOnExit, int64(i))
		},
		log: testingZapLogger(t),
		config: config{
			ListenAddr:    ":0",
			DataDirectory: "",
			Repos:         "git@github.com:cresta/gitdb-reference.git",
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
	t.Run("not_found", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/unknown", sendPort))
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	require.NoError(t, s.server.Shutdown(context.Background()))
	wg.Wait()
}

func requiredRead(t *testing.T, r io.Reader) string {
	b, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}
