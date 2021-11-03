// +build integration

package gitdb

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/cresta/gitdb/internal/gitdb/goget"

	"github.com/cresta/gitdb/internal/gitdb/tracing"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/cresta/gitdb/internal/testhelp"

	"github.com/stretchr/testify/require"
)

func cleanupRepo(t *testing.T, c *goget.GitCheckout) {
	require.NotEqual(t, "/", c.AbsPath())
	require.NotEmpty(t, c.AbsPath())
	require.True(t, strings.HasPrefix(c.AbsPath(), os.TempDir()))
	t.Log("Deleting all of", c.AbsPath())
	require.NoError(t, os.RemoveAll(c.AbsPath()))
}

func withRepo(t *testing.T) *goget.GitCheckout {
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
	g := goget.GitOperator{
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
	f, err := c.LsFiles(context.Background(), "master")
	require.NoError(t, err)
	require.Greater(t, len(f), 1)
}

func statsToName(in []goget.FileStat) []string {
	ret := make([]string, 0, len(in))
	for _, i := range in {
		ret = append(ret, i.Name)
	}
	return ret
}

func TestZipContent(t *testing.T) {
	c := withRepo(t)
	defer cleanupRepo(t, c)
	ctx := context.Background()
	var buf bytes.Buffer
	_, err := c.ZipContent(ctx, &buf, "adir/", "master")
	require.NoError(t, err)

	// Now try unzipping to make sure it matches
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	require.Equal(t, 3, len(r.File))
	findFile := func(files []*zip.File, name string) *zip.File {
		for _, f := range files {
			if f.Name == name {
				return f
			}
		}
		return nil
	}
	f := findFile(r.File, "subdir/subdir_file.txt")
	require.NotNil(t, f)
	rc, err := f.Open()
	require.NoError(t, err)
	d, err := ioutil.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "file1\n", string(d))
}

func TestGitgitCheckout_LsDir_subdir(t *testing.T) {
	c := withRepo(t)
	defer cleanupRepo(t, c)
	verifyDir := func(dir string, expected []string) func(t *testing.T) {
		return func(t *testing.T) {
			f, err := c.LsDir(context.Background(), dir, "master")
			require.NoError(t, err)
			require.Equal(t, expected, statsToName(f))
		}
	}
	verifyError := func(dir string, expected error) func(t *testing.T) {
		return func(t *testing.T) {
			_, err := c.LsDir(context.Background(), dir, "master")
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

func TestGitgitCheckout_FileContent(t *testing.T) {
	defaultCheckout := withRepo(t)
	defer cleanupRepo(t, defaultCheckout)
	mustExist := func(c *goget.GitCheckout, name string, expectedContent string, ref string) func(t *testing.T) {
		return func(t *testing.T) {
			content, err := c.GetFile(context.Background(), ref, name)
			require.NoError(t, err)
			var b bytes.Buffer
			numBytes, err := content.WriteTo(&b)
			require.NoError(t, err)
			require.Equal(t, int(numBytes), len(expectedContent))
			require.Equal(t, expectedContent, b.String())
		}
	}

	mustNotExist := func(c *goget.GitCheckout, name string, ref string) func(t *testing.T) {
		return func(t *testing.T) {
			badContent, err := c.GetFile(context.Background(), ref, name)
			require.Error(t, err)
			require.Nil(t, badContent)
		}
	}

	t.Run("file_in_dir", mustExist(defaultCheckout, "adir/file_in_directory.txt", "file_in_directory\n", "master"))
	t.Run("on_master", mustExist(defaultCheckout, "on_master.txt", "true\n", "master"))
	t.Run("on_staging", mustExist(defaultCheckout, "on_staging.txt", "staging\n", "staging"))

	t.Run("bad_name", mustNotExist(defaultCheckout, "must_not_exist", "master"))
	t.Run("bad_name_for_master", mustNotExist(defaultCheckout, "on_master.txt", "staging"))
}
