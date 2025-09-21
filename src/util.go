package main

import "io"

type BoundedReader struct {
	inner io.Reader
	fuel  int64
	err   error
}

func ReadAtMost(reader io.Reader, count int64, err error) io.Reader {
	return &BoundedReader{reader, count, err}
}

func (reader *BoundedReader) Read(dest []byte) (count int, err error) {
	if reader.fuel <= 0 {
		return 0, reader.err
	}
	if int64(len(dest)) > reader.fuel {
		dest = dest[0:reader.fuel]
	}
	count, err = reader.inner.Read(dest)
	reader.fuel -= int64(count)
	return
}
