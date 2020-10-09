package gitdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/cresta/gitdb/internal/log"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

type GitOperator struct {
	Log *log.Logger
}

func (g *GitOperator) Clone(ctx context.Context, into string, remoteURL string, auth transport.AuthMethod) (*GitCheckout, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "clone")
	defer span.Finish()
	repo, err := git.PlainCloneContext(ctx, into, true, &git.CloneOptions{
		URL:   remoteURL,
		Depth: 1,
		Auth:  auth,
	})
	if err != nil {
		return nil, err
	}
	return &GitCheckout{
		repo:      repo,
		absPath:   into,
		auth:      auth,
		remoteURL: remoteURL,
		log:       g.Log.With(zap.String("repo", remoteURL)),
	}, nil
}

type GitCheckout struct {
	absPath   string
	repo      *git.Repository
	log       *log.Logger
	ref       *plumbing.Reference
	remoteURL string
	auth      transport.AuthMethod
}

func (g *GitCheckout) Refresh(ctx context.Context) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "refresh")
	defer span.Finish()
	err := g.repo.FetchContext(ctx, &git.FetchOptions{
		Auth: g.auth,
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return fmt.Errorf("unable to refresh repository: %w", err)
}

func (g *GitCheckout) AbsPath() string {
	return g.absPath
}

func (g *GitCheckout) reference() (*plumbing.Reference, error) {
	if g.ref != nil {
		return g.ref, nil
	}
	return g.repo.Head()
}

func (g *GitCheckout) RemoteExists(remote string) bool {
	r, err := g.repo.Remote(remote)
	if err != nil {
		return false
	}
	return r != nil
}

func (g *GitCheckout) WithReference(ctx context.Context, refName string) (*GitCheckout, error) {
	r, err := g.repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil {
		return nil, fmt.Errorf("unable to resolve ref %s: %w", refName, err)
	}
	g.log.Info(ctx, "Switched hash", zap.String("hash", r.Hash().String()))
	return &GitCheckout{
		auth:      g.auth,
		absPath:   g.absPath,
		remoteURL: g.remoteURL,
		repo:      g.repo,
		log:       g.log.With(zap.String("ref", refName)),
		ref:       r,
	}, nil
}

func (g *GitCheckout) LsFiles(ctx context.Context) ([]string, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "ls_files")
	defer span.Finish()
	g.log.Info(ctx, "asked to list files")
	defer g.log.Info(ctx, "list done")
	w, err := g.reference()
	if err != nil {
		return nil, fmt.Errorf("unable to get repo head: %w", err)
	}
	t, err := g.repo.CommitObject(w.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to make tree object for hash %s: %w", w.Hash(), err)
	}
	iter, err := t.Files()
	if err != nil {
		return nil, fmt.Errorf("unable to get files for hash: %w", err)
	}
	ret := make([]string, 0)
	if err := iter.ForEach(func(file *object.File) error {
		ret = append(ret, file.Name)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("uanble to list all files of hash: %w", err)
	}
	return ret, nil
}

type FileStat struct {
	Name string
	Mode uint32
	Hash string
}

func (g *GitCheckout) LsDir(ctx context.Context, dir string) (retStat []FileStat, retErr error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "ls_files")
	defer span.Finish()
	g.log.Info(ctx, "asked to list files")
	defer func() {
		g.log.Info(ctx, "list done", zap.Error(retErr))
	}()
	w, err := g.reference()
	if err != nil {
		return nil, fmt.Errorf("unable to get repo head: %w", err)
	}
	co, err := g.repo.CommitObject(w.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to make commit object for hash %s: %w", w.Hash(), err)
	}
	t, err := co.Tree()
	if err != nil {
		return nil, fmt.Errorf("unable to make tree object for hash %s: %w", co.Hash, err)
	}
	te := t
	if dir != "" {
		te, err = t.Tree(dir)
		if err != nil {
			return nil, fmt.Errorf("unable to find entry named %s: %w", dir, err)
		}
	}
	ret := make([]FileStat, 0)
	for _, e := range te.Entries {
		ret = append(ret, FileStat{
			Name: e.Name,
			Mode: uint32(e.Mode),
			Hash: e.Hash.String(),
		})
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].Name < ret[j].Name
	})
	return ret, nil
}

// Will eventually want to cache this
func (g *GitCheckout) FileContent(ctx context.Context, fileName string) (io.WriterTo, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "file_content")
	defer span.Finish()
	g.log.Info(ctx, "asked to fetch file", zap.String("file_name", fileName))
	defer g.log.Info(ctx, "fetch done")
	w, err := g.reference()
	if err != nil {
		return nil, fmt.Errorf("unable to get repo head: %w", err)
	}
	t, err := g.repo.CommitObject(w.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to make tree object for hash %s: %w", w.Hash(), err)
	}
	f, err := t.File(fileName)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch file %s: %w", fileName, err)
	}
	return &readerWriterTo{
		f: f,
		z: g.log.With(zap.String("file_name", fileName)),
	}, nil
}

type readerWriterTo struct {
	f *object.File
	z *log.Logger
}

func (r *readerWriterTo) WriteTo(w io.Writer) (n int64, err error) {
	rd, err := r.f.Reader()
	if err != nil {
		return 0, fmt.Errorf("unable to make reader : %w", err)
	}
	defer func() {
		r.z.IfErr(rd.Close()).Warn(context.Background(), "unable to close file object")
	}()
	return io.Copy(w, rd)
}

var _ io.WriterTo = &readerWriterTo{}
