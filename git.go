package main

import (
	"context"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
	"io"
)

type gitOperator struct {
	log *zap.Logger
}

func (g *gitOperator) clone(ctx context.Context, into string, remoteURL string) (Checkout, error) {
	repo, err := git.PlainCloneContext(ctx, into, false, &git.CloneOptions{
		URL:   remoteURL,
		Depth: 1,
	})
	if err != nil {
		return nil, err
	}
	return &gitCheckout{
		repo:    repo,
		absPath: into,
		log:     g.log.With(zap.String("repo", remoteURL)),
	}, nil
}

type gitCheckout struct {
	absPath string
	repo    *git.Repository
	log     *zap.Logger
}

type Checkout interface {
	LsFiles() ([]string, error)
	FileContent(fileName string) (io.WriterTo, error)
}

func (g *gitCheckout) LsFiles() ([]string, error) {
	g.log.Info("asked to list files")
	defer g.log.Info("list done")
	w, err := g.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to get repo head: %v", err)
	}
	t, err := g.repo.CommitObject(w.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to make tree object for hash %s: %v", w.Hash(), err)
	}
	iter, err := t.Files()
	if err != nil {
		return nil, fmt.Errorf("unable to get files for hash: %v", err)
	}
	ret := make([]string, 0)
	if err := iter.ForEach(func(file *object.File) error {
		ret = append(ret, file.Name)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("uanble to list all files of hash: %v", err)
	}
	return ret, nil
}

func (g *gitCheckout) FileContent(fileName string) (io.WriterTo, error) {
	g.log.Info("asked to fetch file", zap.String("file_name", fileName))
	defer g.log.Info("fetch done")
	w, err := g.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to get repo head: %v", err)
	}
	t, err := g.repo.CommitObject(w.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to make tree object for hash %s: %v", w.Hash(), err)
	}
	f, err := t.File(fileName)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch file %s: %v", fileName, err)
	}
	return &readerWriterTo{
		f: f,
		z: g.log.With(zap.String("file_name", fileName)),
	}, nil
}

type readerWriterTo struct {
	f *object.File
	z *zap.Logger
}

func (r *readerWriterTo) WriteTo(w io.Writer) (n int64, err error) {
	rd, err := r.f.Reader()
	if err != nil {
		return 0, fmt.Errorf("unable to make reader : %v", err)
	}
	defer func() {
		if err := rd.Close(); err != nil {
			r.z.Warn("unable to close file object", zap.Error(err))
		}
	}()
	return io.Copy(w, rd)
}

var _ io.WriterTo = &readerWriterTo{}
