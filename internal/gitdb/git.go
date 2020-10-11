package gitdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/cresta/gitdb/internal/gitdb/tracing"

	"github.com/cresta/gitdb/internal/log"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
)

type GitOperator struct {
	Log    *log.Logger
	Tracer tracing.Tracing
}

func (g *GitOperator) Clone(ctx context.Context, into string, remoteURL string, auth transport.AuthMethod) (*GitCheckout, error) {
	var ret *GitCheckout
	err := g.Tracer.StartSpanFromContext(ctx, tracing.SpanConfig{OperationName: "clone"}, func(ctx context.Context) error {
		repo, err := git.PlainCloneContext(ctx, into, true, &git.CloneOptions{
			URL:   remoteURL,
			Depth: 1,
			Auth:  auth,
		})
		if err != nil {
			return err
		}
		ret = &GitCheckout{
			repo:      repo,
			absPath:   into,
			auth:      auth,
			tracing:   g.Tracer,
			remoteURL: remoteURL,
			log:       g.Log.With(zap.String("repo", remoteURL)),
		}
		return nil
	})
	return ret, err
}

type GitCheckout struct {
	absPath   string
	tracing   tracing.Tracing
	repo      *git.Repository
	log       *log.Logger
	ref       *plumbing.Reference
	remoteURL string
	auth      transport.AuthMethod
}

func (g *GitCheckout) Refresh(ctx context.Context) error {
	return g.tracing.StartSpanFromContext(ctx, tracing.SpanConfig{OperationName: "refresh"}, func(ctx context.Context) error {
		g.tracing.AttachTag(ctx, "git.remote_url", g.remoteURL)
		err := g.repo.FetchContext(ctx, &git.FetchOptions{
			Auth: g.auth,
		})
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil
		}
		return fmt.Errorf("unable to refresh repository: %w", err)
	})
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
	g.log.Debug(ctx, "Switched hash", zap.String("hash", r.Hash().String()))
	return &GitCheckout{
		auth:      g.auth,
		absPath:   g.absPath,
		remoteURL: g.remoteURL,
		repo:      g.repo,
		tracing:   g.tracing,
		log:       g.log.With(zap.String("ref", refName)),
		ref:       r,
	}, nil
}

func (g *GitCheckout) LsFiles(ctx context.Context) ([]string, error) {
	var ret []string
	err := g.tracing.StartSpanFromContext(ctx, tracing.SpanConfig{OperationName: "ls_files"}, func(ctx context.Context) error {
		g.log.Debug(ctx, "asked to list files")
		defer g.log.Debug(ctx, "list done")
		w, err := g.reference()
		if err != nil {
			return fmt.Errorf("unable to get repo head: %w", err)
		}
		t, err := g.repo.CommitObject(w.Hash())
		if err != nil {
			return fmt.Errorf("unable to make tree object for hash %s: %w", w.Hash(), err)
		}
		iter, err := t.Files()
		if err != nil {
			return fmt.Errorf("unable to get files for hash: %w", err)
		}
		ret = make([]string, 0)
		if err := iter.ForEach(func(file *object.File) error {
			ret = append(ret, file.Name)
			return nil
		}); err != nil {
			return fmt.Errorf("uanble to list all files of hash: %w", err)
		}
		return nil
	})
	return ret, err
}

type FileStat struct {
	Name string
	Mode uint32
	Hash string
}

func (g *GitCheckout) LsDir(ctx context.Context, dir string) (retStat []FileStat, retErr error) {
	g.log.Debug(ctx, "asked to list files")
	defer func() {
		g.log.Debug(ctx, "list done", zap.Error(retErr))
	}()
	retErr = g.tracing.StartSpanFromContext(ctx, tracing.SpanConfig{OperationName: "ls_dir"}, func(ctx context.Context) error {
		w, err := g.reference()
		if err != nil {
			return fmt.Errorf("unable to get repo head: %w", err)
		}
		co, err := g.repo.CommitObject(w.Hash())
		if err != nil {
			return fmt.Errorf("unable to make commit object for hash %s: %w", w.Hash(), err)
		}
		t, err := co.Tree()
		if err != nil {
			return fmt.Errorf("unable to make tree object for hash %s: %w", co.Hash, err)
		}
		te := t
		if dir != "" {
			te, err = t.Tree(dir)
			if err != nil {
				return fmt.Errorf("unable to find entry named %s: %w", dir, err)
			}
		}
		retStat = make([]FileStat, 0)
		for _, e := range te.Entries {
			retStat = append(retStat, FileStat{
				Name: e.Name,
				Mode: uint32(e.Mode),
				Hash: e.Hash.String(),
			})
		}
		sort.Slice(retStat, func(i, j int) bool {
			return retStat[i].Name < retStat[j].Name
		})
		return nil
	})
	return retStat, retErr
}

// Will eventually want to cache this
func (g *GitCheckout) FileContent(ctx context.Context, fileName string) (io.WriterTo, error) {
	var ret io.WriterTo
	err := g.tracing.StartSpanFromContext(ctx, tracing.SpanConfig{OperationName: "file_content"}, func(ctx context.Context) error {
		g.log.Debug(ctx, "asked to fetch file", zap.String("file_name", fileName))
		defer g.log.Debug(ctx, "fetch done")
		w, err := g.reference()
		if err != nil {
			return fmt.Errorf("unable to get repo head: %w", err)
		}
		t, err := g.repo.CommitObject(w.Hash())
		if err != nil {
			return fmt.Errorf("unable to make tree object for hash %s: %w", w.Hash(), err)
		}
		f, err := t.File(fileName)
		if err != nil {
			return fmt.Errorf("unable to fetch file %s: %w", fileName, err)
		}
		ret = &readerWriterTo{
			f: f,
			z: g.log.With(zap.String("file_name", fileName)),
		}
		return nil
	})
	return ret, err
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
