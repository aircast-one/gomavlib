package gomavlib

import (
	"io"
)

type removeCloser struct {
	wrapped io.ReadWriteCloser
}

func (r *removeCloser) Read(p []byte) (int, error) {
	return r.wrapped.Read(p)
}

func (r *removeCloser) Write(p []byte) (int, error) {
	return r.wrapped.Write(p)
}

func (r *removeCloser) Close() error {
	return nil
}
