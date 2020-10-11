// +build integration

package gitdb

import (
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/cresta/gitdb/internal/gitdb/tracing"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cresta/gitdb/internal/testhelp"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

var stagingRef = string(plumbing.NewRemoteReferenceName("origin", "staging"))

func cleanupRepo(t *testing.T, c *GitCheckout) {
	require.NotEqual(t, "/", c.AbsPath())
	require.NotEmpty(t, c.AbsPath())
	require.True(t, strings.HasPrefix(c.AbsPath(), os.TempDir()))
	t.Log("Deleting all of", c.AbsPath())
	require.NoError(t, os.RemoveAll(c.AbsPath()))
}

func withRepo(t *testing.T) *GitCheckout {
	ctx := context.Background()
	repo := os.Getenv("TEST_REPO")
	if repo == "" {
		repo = "git@github.com:cresta/gitdb-reference.git"
	}
	require.NotEmpty(t, repo)
	into, err := ioutil.TempDir("", "TestClone")
	require.NoError(t, err)
	require.NotEmpty(t, into)
	t.Log("Clone into", into)
	g := GitOperator{
		Log:    testhelp.ZapTestingLogger(t),
		Tracer: tracing.Noop{},
	}
	c, err := g.Clone(ctx, into, repo, nil)
	require.NoError(t, err)
	require.NotNil(t, c)
	return c
}

func TestGitgitCheckout_LsFiles(t *testing.T) {
	c := withRepo(t)
	defer cleanupRepo(t, c)
	f, err := c.LsFiles(context.Background())
	require.NoError(t, err)
	require.Greater(t, len(f), 1)
}

func statsToName(in []FileStat) []string {
	ret := make([]string, 0, len(in))
	for _, i := range in {
		ret = append(ret, i.Name)
	}
	return ret
}

func TestGitgitCheckout_LsDir_subdir(t *testing.T) {
	c := withRepo(t)
	defer cleanupRepo(t, c)
	verifyDir := func(dir string, expected []string) func(t *testing.T) {
		return func(t *testing.T) {
			f, err := c.LsDir(context.Background(), dir)
			require.NoError(t, err)
			require.Equal(t, expected, statsToName(f))
		}
	}
	verifyError := func(dir string, expected error) func(t *testing.T) {
		return func(t *testing.T) {
			_, err := c.LsDir(context.Background(), dir)
			require.Error(t, err)
			require.True(t, errors.Is(err, expected))
		}
	}
	t.Run("root", verifyDir("", []string{"README.md", "adir", "on_master.txt"}))
	t.Run("dir", verifyDir("adir", []string{"file_in_directory.txt", "subdir"}))
	t.Run("subdir", verifyDir("adir/subdir", []string{"subdir_file.txt", "subdir_file2.txt"}))
	t.Run("missing_dir", verifyError("notadir", object.ErrDirectoryNotFound))
}

func TestGitCheckout_Refresh(t *testing.T) {
	c := withRepo(t)
	defer cleanupRepo(t, c)
	err := c.Refresh(context.Background())
	require.NoError(t, err)
}

func mustResolve(t *testing.T, c *GitCheckout, ref string) *GitCheckout {
	ret, err := c.WithReference(context.Background(), ref)
	require.NoError(t, err)
	return ret
}

func TestGitgitCheckout_FileContent(t *testing.T) {
	defaultCheckout := withRepo(t)
	defer cleanupRepo(t, defaultCheckout)
	staging := mustResolve(t, defaultCheckout, stagingRef)
	mustExist := func(c *GitCheckout, name string, expectedContent string) func(t *testing.T) {
		return func(t *testing.T) {
			content, err := c.FileContent(context.Background(), name)
			require.NoError(t, err)
			var b bytes.Buffer
			numBytes, err := content.WriteTo(&b)
			require.NoError(t, err)
			require.Equal(t, int(numBytes), len(expectedContent))
			require.Equal(t, expectedContent, b.String())
		}
	}

	mustNotExist := func(c *GitCheckout, name string) func(t *testing.T) {
		return func(t *testing.T) {
			badContent, err := c.FileContent(context.Background(), name)
			require.Error(t, err)
			require.Nil(t, badContent)
		}
	}

	t.Run("file_in_dir", mustExist(defaultCheckout, "adir/file_in_directory.txt", "file_in_directory\n"))
	t.Run("on_master", mustExist(defaultCheckout, "on_master.txt", "true\n"))
	t.Run("on_staging", mustExist(staging, "on_staging.txt", "staging\n"))

	t.Run("bad_name", mustNotExist(defaultCheckout, "must_not_exist"))
	t.Run("bad_name_for_master", mustNotExist(staging, "on_master.txt"))
}
