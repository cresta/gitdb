package main

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"io/ioutil"
	"os"
	"testing"
)

func TestClone(t *testing.T) {
	ctx := context.Background()
	repo := os.Getenv("TEST_REPO")
	require.NotEmpty(t, repo)
	into, err := ioutil.TempDir("", "TestClone")
	require.NoError(t, err)
	require.NotEmpty(t, into)
	defer func() {
		t.Log("Deleting all of", into)
		require.NoError(t, os.RemoveAll(into))
	}()
	t.Log("Clone into", into)
	g := gitOperator{
		log: zap.L(),
	}
	c, err := g.clone(ctx, into, repo)
	require.NoError(t, err)
	require.NotNil(t, c)
	f, err := c.LsFiles()
	require.NoError(t, err)
	require.Greater(t, len(f), 1)
	content, err := c.FileContent("README.md")
	require.NoError(t, err)
	var b bytes.Buffer
	numBytes, err := content.WriteTo(&b)
	require.NoError(t, err)
	require.Greater(t, numBytes, int64(1))
	require.Equal(t, numBytes, int64(len(b.String())))

	badContent, err := c.FileContent("does_not_exist")
	require.Error(t, err)
	require.Nil(t, badContent)
}
