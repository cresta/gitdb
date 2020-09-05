package main

import (
	"context"
	"github.com/go-git/go-git/v5"
)

type gitOperator struct {
}

func (g *gitOperator) clone(ctx context.Context, into string, remoteURL string) (Checkout, error) {
	repo, err := git.PlainCloneContext(ctx, into, true, &git.CloneOptions{
		URL: remoteURL,
	})
	if err != nil {
		return nil, err
	}
	return &gitCheckout{
		repo:    repo,
		absPath: into,
	}, nil
}

type gitCheckout struct {
	absPath string
	repo    *git.Repository
}

type Checkout interface {
	LsFiles(ctx context.Context) ([]string, error)
}

func (g *gitCheckout) LsFiles(ctx context.Context) ([]string, error) {
	return nil, nil
}
