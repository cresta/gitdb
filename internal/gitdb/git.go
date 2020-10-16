package gitdb

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

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
		var progress bytes.Buffer
		repo, err := git.PlainCloneContext(ctx, into, true, &git.CloneOptions{
			URL:      remoteURL,
			Auth:     attachContextToAuth(ctx, auth),
			Progress: &progress,
		})
		if err != nil {
			g.Log.Warn(ctx, "unable to clone", zap.Stringer("progress", &progress))
			return err
		}
		g.Log.Debug(ctx, "clone finished", zap.Stringer("progress", &progress))
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
		var progress bytes.Buffer
		g.tracing.AttachTag(ctx, "git.remote_url", g.remoteURL)
		err := g.repo.FetchContext(ctx, &git.FetchOptions{
			Auth:     attachContextToAuth(ctx, g.auth),
			Progress: &progress,
		})
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			g.log.Debug(ctx, "fetch finished", zap.Stringer("progress", &progress))
			return nil
		}
		g.log.Warn(ctx, "unable to fetch", zap.Stringer("progress", &progress))
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

func ZipContent(ctx context.Context, into io.Writer, prefix string, from *GitCheckout) (int, error) {
	w := zip.NewWriter(into)
	files, err := from.LsFiles(ctx)
	prefix = strings.Trim(prefix, "/")
	if err != nil {
		return 0, fmt.Errorf("unable to list files: %w", err)
	}
	numFiles := 0
	for _, file := range files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		filePath := file[len(prefix):]
		wf, err := w.Create(strings.TrimPrefix(filePath, "/"))
		if err != nil {
			return numFiles, fmt.Errorf("unable to create file at path %s: %w", filePath, err)
		}
		wt, err := from.FileContent(ctx, file)
		if err != nil {
			return numFiles, fmt.Errorf("unable to get file content for %s: %w", file, err)
		}
		if _, err := wt.WriteTo(wf); err != nil {
			return numFiles, fmt.Errorf("unable to write file named %s: %w", file, err)
		}
		numFiles++
	}
	if err := w.Close(); err != nil {
		return numFiles, fmt.Errorf("unable to close zip: %w", err)
	}
	return numFiles, nil
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

func WrapGitProtocols(t tracing.Tracing) {
	for key, protocol := range client.Protocols {
		if _, ok := client.Protocols[key].(*LoggedClient); ok {
			continue
		}
		client.Protocols[key] = &LoggedClient{
			Wrapped: protocol,
			Tracing: t,
		}
	}
}

type LoggedClient struct {
	Wrapped transport.Transport
	Tracing tracing.Tracing
}

var _ transport.Transport = &LoggedClient{}

func (l *LoggedClient) NewUploadPackSession(endpoint *transport.Endpoint, authMethod transport.AuthMethod) (transport.UploadPackSession, error) {
	var ret transport.UploadPackSession
	err := l.Tracing.StartSpanFromContext(contextFromAuth(authMethod), tracing.SpanConfig{OperationName: "NewUploadPackSession"}, func(ctx context.Context) error {
		authMethod = unwrapAuth(authMethod)
		l.Tracing.AttachTag(ctx, "git.upload_pack.endpoint", endpoint.String())
		if authMethod != nil {
			l.Tracing.AttachTag(ctx, "git.auth", authMethod.Name())
		}
		var retErr error
		ret, retErr = l.Wrapped.NewUploadPackSession(endpoint, authMethod)
		return retErr
	})
	return ret, err
}

func (l *LoggedClient) NewReceivePackSession(endpoint *transport.Endpoint, authMethod transport.AuthMethod) (transport.ReceivePackSession, error) {
	var ret transport.ReceivePackSession
	err := l.Tracing.StartSpanFromContext(contextFromAuth(authMethod), tracing.SpanConfig{OperationName: "NewReceivePackSession"}, func(ctx context.Context) error {
		authMethod = unwrapAuth(authMethod)
		l.Tracing.AttachTag(ctx, "git.recv_pack.endpoint", endpoint.String())
		if authMethod != nil {
			l.Tracing.AttachTag(ctx, "git.auth", authMethod.Name())
		}
		var retErr error
		ret, retErr = l.Wrapped.NewReceivePackSession(endpoint, authMethod)
		return retErr
	})
	return ret, err
}

type ContextCurried struct {
	ctx context.Context
}

func (c *ContextCurried) Ctx() context.Context {
	return c.ctx
}

type ContextCurriedAuth struct {
	ContextCurried
	transport.AuthMethod
}

func (c *ContextCurriedAuth) Unwrap() transport.AuthMethod {
	return c.AuthMethod
}

type ContextCurriedSSHAuth struct {
	ContextCurried
	ssh.AuthMethod
}

func (c *ContextCurriedSSHAuth) Unwrap() transport.AuthMethod {
	return c.AuthMethod
}

// https://github.com/go-git/go-git/issues/185
var _ ssh.AuthMethod = &ContextCurriedSSHAuth{}

func attachContextToAuth(ctx context.Context, auth transport.AuthMethod) transport.AuthMethod {
	if sshAuth, ok := auth.(ssh.AuthMethod); ok {
		return &ContextCurriedSSHAuth{
			AuthMethod: sshAuth,
			ContextCurried: ContextCurried{
				ctx: ctx,
			},
		}
	}
	return &ContextCurriedAuth{
		ContextCurried: ContextCurried{
			ctx: ctx,
		},
		AuthMethod: auth,
	}
}

func unwrapAuth(t transport.AuthMethod) transport.AuthMethod {
	type unwrapable interface {
		Unwrap() transport.AuthMethod
	}
	if root, ok := t.(unwrapable); ok {
		return root.Unwrap()
	}
	return t
}

func contextFromAuth(a transport.AuthMethod) context.Context {
	if a == nil {
		return context.Background()
	}
	type ctx interface {
		Ctx() context.Context
	}
	if obj, ok := a.(ctx); ok {
		return obj.Ctx()
	}
	return context.Background()
}
