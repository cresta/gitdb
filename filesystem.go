package main

import (
	"io"
)

type File interface {
	io.ReaderFrom
	FullPath() string
}

type Directory interface {
	List() ([]File, error)
	FullPath() string
}
